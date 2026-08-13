package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/handler"
	"image-backend/internal/storage"
)

// recordingStore 记录每一次 Put 的入参。
//
// 断言"存储收到了什么"而不只是"接口返回了什么"，是这一组测试的关键：上传接口最
// 要紧的两条不变量（拒绝非图片时**根本没有写入**、落地扩展名取自嗅探而非声明值）
// 从响应体上完全看不出来。一个"先写后校验"的实现同样返回 400，但对象已经挂在我们
// 自己的域名下了。
type recordingStore struct {
	calls []recordedPut
	err   error
}

type recordedPut struct {
	key         string
	contentType string
	body        []byte
}

func (s *recordingStore) Put(_ context.Context, key, contentType string, body []byte) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.calls = append(s.calls, recordedPut{key: key, contentType: contentType, body: body})
	return storage.PublicURL("https://img.example.com", key), nil
}

const refPath = "/api/v1/uploads/reference"

// dataURL 拼一个 data URL。declared 是**声明**的类型，可以与 body 的真实类型不一致
// ——那正是需要被测的情况。
func dataURL(declared string, body []byte) string {
	return "data:" + declared + ";base64," + base64.StdEncoding.EncodeToString(body)
}

var (
	pngBody  = []byte("\x89PNG\r\n\x1a\n")
	jpegBody = []byte("\xff\xd8\xff\xe0")
	webpBody = []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
)

// setupUploadRouter 带一个假存储的路由。
//
// 自己拼 NewRouter 而不是走 setupRouterWithDB：后者的 opts 是改写 *config.Config 的，
// 而这里要注入的是 RouterOption。同 stripe_events_test.go 注入假 SubscriptionFetcher
// 的做法。
func setupUploadRouter(t *testing.T) (*gin.Engine, *gorm.DB, *recordingStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	store := &recordingStore{}
	cfg := &config.Config{JWTSecret: "test-secret", ConfigEncryptionKey: testConfigEncryptionKey}
	r := NewRouter(db, cfg, WithReferenceStore(handler.ReferenceStore{
		Store:      store,
		PublicBase: "https://img.example.com",
	}))
	return r, db, store
}

func uploadKeyOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v; body=%s", err, w.Body.String())
	}
	return body.Key
}

func TestUploadReferenceRequiresAuth(t *testing.T) {
	r, _, _ := setupUploadRouter(t)
	w := postJSON(r, refPath, `{"image":"`+dataURL("image/png", pngBody)+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("未认证应当 401，得到 %d: %s", w.Code, w.Body.String())
	}
}

// TestUploadReferenceRejectsNonJSONContentType 钉住 JSONOnly 真的挂在这条路由上。
//
// **这是全组最重要的一条。** 它守的是一个跨域攻击面：multipart/form-data 与
// text/plain 一样是 CORS 简单请求类型、不触发预检，所以一个不挂 JSONOnly 的
// 文件上传端点等于开一个跨域页面可以直接打的、有副作用的入口。
// authed 组整体没挂 JSONOnly，这条路由是单独挂的——最容易在重构时被弄丢。
func TestUploadReferenceRejectsNonJSONContentType(t *testing.T) {
	r, _, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-ct@example.com", "secret12345")

	for _, ct := range []string{"text/plain", "multipart/form-data; boundary=x", ""} {
		req := httptest.NewRequest(http.MethodPost, refPath,
			strings.NewReader(`{"image":"`+dataURL("image/png", pngBody)+`"}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type=%q 应当 415（JSONOnly 没挂上？），得到 %d: %s",
				ct, w.Code, w.Body.String())
		}
		if len(store.calls) != 0 {
			t.Fatalf("Content-Type=%q 被拒时不该写入存储，却写了 %d 次", ct, len(store.calls))
		}
	}
}

func TestUploadReferenceStoresImageAndReturnsKey(t *testing.T) {
	r, db, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-ok@example.com", "secret12345")
	userID := userIDOf(t, db, "up-ok@example.com")

	for _, tc := range []struct {
		name    string
		body    []byte
		wantCT  string
		wantExt string
	}{
		{"png", pngBody, "image/png", "png"},
		{"jpeg", jpegBody, "image/jpeg", "jpg"},
		{"webp", webpBody, "image/webp", "webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(store.calls)
			w := postAuthed(r, token, refPath, `{"image":"`+dataURL(tc.wantCT, tc.body)+`"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("应当 200，得到 %d: %s", w.Code, w.Body.String())
			}
			key := uploadKeyOf(t, w)

			// 键必须带调用者的 userID 段：生成接口靠它确认"这是你自己上传的图"，
			// 没有这一段的话 A 能引用 B 上传的图。
			wantPrefix := fmt.Sprintf("ref/%d/", userID)
			if !strings.HasPrefix(key, wantPrefix) {
				t.Errorf("键 %q 应当以 %q 开头", key, wantPrefix)
			}
			if !strings.HasSuffix(key, "."+tc.wantExt) {
				t.Errorf("键 %q 应当以 .%s 结尾", key, tc.wantExt)
			}
			// **响应里不能有 URL。** 返回 URL 就意味着生成接口那一侧要接收并校验
			// 用户传回来的 URL，而那条校验路径是 SSRF 的来源（见 uploads.go 的注释）。
			if strings.Contains(w.Body.String(), "http") {
				t.Errorf("响应不该包含任何 URL，得到 %s", w.Body.String())
			}

			if len(store.calls) != before+1 {
				t.Fatalf("应当写入一次，实际写了 %d 次", len(store.calls)-before)
			}
			got := store.calls[len(store.calls)-1]
			if got.key != key {
				t.Errorf("写入的键 %q 与返回的键 %q 不一致", got.key, key)
			}
			if got.contentType != tc.wantCT {
				t.Errorf("写入的 content-type 是 %q，想要 %q", got.contentType, tc.wantCT)
			}
		})
	}
}

// TestUploadReferenceUsesSniffedTypeNotDeclared 声明什么都不算数，字节才算数。
//
// 这条同时钉住两件事：声明值被忽略（安全），以及落地扩展名取自嗅探结果（正确性
// ——扩展名错了的对象在浏览器里可能不显示，而库里看起来一切正常）。
func TestUploadReferenceUsesSniffedTypeNotDeclared(t *testing.T) {
	r, _, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-sniff@example.com", "secret12345")

	// 声明 png，实际是 jpeg。
	w := postAuthed(r, token, refPath, `{"image":"`+dataURL("image/png", jpegBody)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应当接受（字节是合法 jpeg），得到 %d: %s", w.Code, w.Body.String())
	}
	key := uploadKeyOf(t, w)
	if !strings.HasSuffix(key, ".jpg") {
		t.Errorf("扩展名应当取嗅探结果 .jpg，得到 %q", key)
	}
	if len(store.calls) != 1 {
		t.Fatalf("应当写入一次，实际 %d 次", len(store.calls))
	}
	if got := store.calls[0].contentType; got != "image/jpeg" {
		t.Errorf("content-type 应当取嗅探结果 image/jpeg，得到 %q", got)
	}
}

// TestUploadReferenceRejectsNonImageWithoutWriting 是这一组里安全性最关键的一条。
//
// 断言**没有写入**，而不只是断言返回 400：一个"先 Put 再校验"的实现同样返回 400，
// 但那个 HTML 文件已经挂在我们自己的 origin 上了——那是 XSS，而接口看起来一切正常。
func TestUploadReferenceRejectsNonImageWithoutWriting(t *testing.T) {
	r, _, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-evil@example.com", "secret12345")

	for _, tc := range []struct {
		name string
		body []byte
		why  string
	}{
		{"html 伪装成 png", []byte("<!DOCTYPE html><script>alert(document.cookie)</script>"),
			"挂在我们自己的 origin 上就是 XSS"},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
			"SVG 是可执行脚本的图片格式，浏览器直接打开会跑里面的 script"},
		{"gif", []byte("GIF89a"), "上游不接受，让它进来只会在生成时失败，而那时已经存进去了"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 声明成 image/png——声明值不构成任何证据，这就是要嗅探的原因。
			w := postAuthed(r, token, refPath, `{"image":"`+dataURL("image/png", tc.body)+`"}`)
			if w.Code != http.StatusBadRequest {
				t.Errorf("应当 400（%s），得到 %d: %s", tc.why, w.Code, w.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("被拒的内容**绝不能**写进存储（%s），却写了 %d 次。"+
					"校验必须发生在 Put 之前", tc.why, len(store.calls))
			}
		})
	}
}

func TestUploadReferenceRejectsMalformedPayloads(t *testing.T) {
	r, _, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-bad@example.com", "secret12345")

	for _, tc := range []struct{ name, body string }{
		{"缺 image 字段", `{}`},
		{"image 为空串", `{"image":""}`},
		{"裸 base64（不是 data URL）", `{"image":"` + base64.StdEncoding.EncodeToString(pngBody) + `"}`},
		{"data URL 缺逗号", `{"image":"data:image/png;base64"}`},
		{"data URL 不是 base64 编码", `{"image":"data:image/png,%89PNG"}`},
		{"声明的类型不是 image/*", `{"image":"` + dataURL("text/html", pngBody) + `"}`},
		{"base64 载荷非法", `{"image":"data:image/png;base64,@@@not-base64@@@"}`},
		{"body 不是合法 JSON", `{"image":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postAuthed(r, token, refPath, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("应当 400，得到 %d: %s", w.Code, w.Body.String())
			}
			if len(store.calls) != 0 {
				t.Fatalf("被拒时不该写入存储，却写了 %d 次", len(store.calls))
			}
		})
	}
}

// TestUploadReferenceRejectsOversizedBeforeDecoding 体积上限必须在**解码之前**生效。
//
// 先解码再判断等于"拦住"发生在内存已经被占掉之后，而这是个任何已登录用户都能反复
// 打的入口。这条测试用一个远超上限的载荷，断言它以 413 被拒。
func TestUploadReferenceRejectsOversizedBeforeDecoding(t *testing.T) {
	r, _, store := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-big@example.com", "secret12345")

	// 2MB 的 base64 字符，超过 maxReferenceDataURLChars（1.4MB）。
	huge := "data:image/png;base64," + strings.Repeat("A", 2_000_000)
	body, err := json.Marshal(map[string]string{"image": huge})
	if err != nil {
		t.Fatalf("构造请求体: %v", err)
	}
	w := postAuthed(r, token, refPath, string(body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超限应当 413，得到 %d: %s", w.Code, w.Body.String())
	}
	if len(store.calls) != 0 {
		t.Errorf("超限时不该写入存储，却写了 %d 次", len(store.calls))
	}
}

// TestUploadReferenceReportsStorageNotConfigured R2 没配好时必须是一个**说得清原因**
// 的 503，而不是 500。
//
// 两者的处置完全不同：500 让人去查代码和日志，而这一条的意思是"去后台把 R2 那五项
// 填上"。本地开发与 e2e 都跑在这个状态下，所以这条路径是常态而非异常。
func TestUploadReferenceReportsStorageNotConfigured(t *testing.T) {
	// 不传 WithReferenceStore：cfg 没有 R2 五项，走 NoopStorage。
	r, _ := setupRouterWithDB(t)
	token := registerAndLogin(t, r, "up-noconf@example.com", "secret12345")

	w := postAuthed(r, token, refPath, `{"image":"`+dataURL("image/png", pngBody)+`"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置存储应当 503（不是 500——那会把排查指向代码而不是配置），得到 %d: %s",
			w.Code, w.Body.String())
	}
	var body struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	// 专用错误码，不复用 50000：前端要据此显示"本部署未开启参考图"而不是通用报错。
	if body.Code != 50302 {
		t.Errorf("错误码应当是 50302，得到 %d: %s", body.Code, w.Body.String())
	}
}

// TestUploadReferenceKeysAreUnpredictable 键必须不可枚举。
//
// 键会以公开 URL 的形式暴露（对象存储桶是公开可读的），可枚举的键等于让任何人
// 遍历别人上传的图片。用户 ID 段是必需的（属主校验靠它），所以随机性必须来自
// 文件名部分。
func TestUploadReferenceKeysAreUnpredictable(t *testing.T) {
	r, _, _ := setupUploadRouter(t)
	token := registerAndLogin(t, r, "up-uniq@example.com", "secret12345")

	seen := map[string]bool{}
	for i := range 5 {
		w := postAuthed(r, token, refPath, `{"image":"`+dataURL("image/png", pngBody)+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次上传失败：%d %s", i+1, w.Code, w.Body.String())
		}
		key := uploadKeyOf(t, w)
		if seen[key] {
			t.Fatalf("键重复了：%q。同一个用户传两张图会互相覆盖", key)
		}
		seen[key] = true
	}
}
