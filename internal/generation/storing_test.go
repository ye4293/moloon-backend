package generation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"image-backend/internal/storage"
)

// fakeInner 是可编排的 inner adapter。
type fakeInner struct {
	url     string
	err     error
	calls   int
	lastReq GenerateRequest
}

func (f *fakeInner) Generate(_ context.Context, req GenerateRequest) (GenerateResult, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return GenerateResult{}, f.err
	}
	return GenerateResult{ImageURL: f.url, UpstreamID: "up-1"}, nil
}

// fakeStore 记录调用并可编排失败。
type fakeStore struct {
	calls    int
	lastKey  string
	lastType string
	lastBody []byte
	err      error
}

func (f *fakeStore) Put(_ context.Context, key, contentType string, body []byte) (string, error) {
	f.calls++
	f.lastKey, f.lastType, f.lastBody = key, contentType, body
	if f.err != nil {
		return "", f.err
	}
	return "https://img.example.com/" + key, nil
}

// pngBytes 是一个最小的合法 PNG 头，足够让 http.DetectContentType 认出 image/png。
var pngBytes = []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32))

// jpegBytes 是一个最小的 JPEG SOI 标记，足够嗅探出 image/jpeg。
var jpegBytes = []byte("\xff\xd8\xff" + strings.Repeat("x", 32))

// webpBytes 是一个最小的可被嗅探的 WebP。
//
// **`VP8 ` 这个 chunk header 是必需的，不能省。** Go 的嗅探表匹配的是
// `RIFF????WEBPVP`——只有 `RIFF....WEBP` 而后面不跟 VP8 chunk 的话，
// DetectContentType 返回 application/octet-stream，于是看起来像"Go 不支持
// webp"，进而让人以为 allowedImageTypes 里的 image/webp 是死代码并把它删掉。
// 真实的 webp 一定是 `VP8 `（有损）、`VP8L`（无损）或 `VP8X`（扩展）之一。
var webpBytes = func() []byte {
	b := append([]byte("RIFF"), 0x20, 0, 0, 0)
	b = append(b, "WEBPVP8 "...)
	return append(b, strings.Repeat("x", 32)...)
}()

func serveBytes(t *testing.T, ct string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStoringAdapterReplacesURLOnSuccess(t *testing.T) {
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/upstream.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-1"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.ImageURL != "https://img.example.com/g/gen-1.png" {
		t.Errorf("ImageURL: got %q", res.ImageURL)
	}
	if !res.Stored {
		t.Error("转存成功必须置 Stored=true，否则前端会对永久链接显示'可能已失效'")
	}
	if store.lastKey != "g/gen-1.png" {
		t.Errorf("key: got %q, want g/gen-1.png", store.lastKey)
	}
	if store.lastType != "image/png" {
		t.Errorf("contentType: got %q", store.lastType)
	}
	if string(store.lastBody) != string(pngBytes) {
		t.Error("上传的字节与下载的不一致")
	}
	// UpstreamID 等既有字段不能被装饰器吃掉。
	if res.UpstreamID != "up-1" {
		t.Errorf("UpstreamID 被丢了: %q", res.UpstreamID)
	}
}

func TestStoringAdapterDegradesWhenStoreFails(t *testing.T) {
	// 这是整个装饰器最重要的一条契约，也是最容易被后人"修"掉的一条。
	// 图已经出了、钱已经花在上游了。因为我们自己的存储抖动就判失败退款，等于把
	// 一次成功且已付费的上游调用白扔，用户还得重排队等 21 秒。
	srv := serveBytes(t, "image/png", pngBytes)
	upstreamURL := srv.URL + "/upstream.png"
	inner := &fakeInner{url: upstreamURL}
	store := &fakeStore{err: errors.New("R2 挂了")}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-2"})
	if err != nil {
		t.Fatalf("转存失败**不能**让生成失败，得到错误: %v", err)
	}
	if res.ImageURL != upstreamURL {
		t.Errorf("降级时要保留上游 URL: got %q", res.ImageURL)
	}
	if res.Stored {
		t.Error("降级时 Stored 必须是 false")
	}
}

func TestStoringAdapterDegradesWhenStorageNotConfigured(t *testing.T) {
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/upstream.png"}
	a := NewStoringAdapter(inner, storage.NoopStorage{})

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-3"})
	if err != nil {
		t.Fatalf("未配置存储不能让生成失败: %v", err)
	}
	if res.Stored {
		t.Error("未配置时 Stored 必须是 false")
	}
}

func TestStoringAdapterSkipsStoreWhenInnerFails(t *testing.T) {
	inner := &fakeInner{err: errors.New("上游拒绝")}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	if _, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-4"}); err == nil {
		t.Fatal("inner 的错误必须原样透出——吞掉它会让用户被扣费却拿不到图")
	}
	if store.calls != 0 {
		t.Errorf("inner 失败时不该调存储，调了 %d 次", store.calls)
	}
}

func TestStoringAdapterSkipsNonHTTPURL(t *testing.T) {
	// StubAdapter 返回的是前端 public/ 下的相对路径，不是可下载的 URL。
	// **必须显式跳过，而不是让它走失败降级**：否则本地开发和 e2e 每次生成都会打
	// 一条转存告警，而那条告警正是生产上唯一提示"这张图一小时后会失效"的信号——
	// 让它变成日常噪音等于把它关掉。
	inner := &fakeInner{url: StubImageURL}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-5"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.ImageURL != StubImageURL {
		t.Errorf("相对路径要原样保留: got %q", res.ImageURL)
	}
	if res.Stored {
		t.Error("没转存就不能说 Stored")
	}
	if store.calls != 0 {
		t.Errorf("相对路径不该调存储，调了 %d 次", store.calls)
	}
}

func TestStoringAdapterRejectsNonImageContent(t *testing.T) {
	// 这个字节流要挂到**我们自己的域名**下。上游若返回 HTML，我们就在自己的
	// origin 上托管了一个别人可控的 HTML 文件——那是 XSS。
	// 注意 handler 谎报 Content-Type 为 image/png，嗅探必须不信它。
	srv := serveBytes(t, "image/png", []byte("<html><script>alert(1)</script></html>"))
	inner := &fakeInner{url: srv.URL + "/evil"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-6"})
	if err != nil {
		t.Fatalf("拒绝非图片内容要走降级而不是报错: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("嗅探出非图片就不能上传，调了 %d 次", store.calls)
	}
	if res.Stored {
		t.Error("没上传就不能说 Stored")
	}
}

func TestStoringAdapterRejectsOversizedImage(t *testing.T) {
	// 无上限地下载进内存是内存耗尽向量：并发几十个请求 + 上游返回一个巨大的
	// 响应，就能把服务打死。
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, storage.MaxImageBytes+1)...)
	srv := serveBytes(t, "image/png", big)
	inner := &fakeInner{url: srv.URL + "/big.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-7"})
	if err != nil {
		t.Fatalf("超限要走降级而不是报错: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("超限就不能上传，调了 %d 次", store.calls)
	}
	if res.Stored {
		t.Error("没上传就不能说 Stored")
	}
}

func TestStoringAdapterDegradesOnDownloadHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	inner := &fakeInner{url: srv.URL + "/gone.png"}
	store := &fakeStore{}
	a := NewStoringAdapter(inner, store)

	res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-8"})
	if err != nil {
		t.Fatalf("下载失败要走降级: %v", err)
	}
	if store.calls != 0 || res.Stored {
		t.Error("下载失败就不该上传、不该置 Stored")
	}
}

func TestStoringAdapterPassesRequestThrough(t *testing.T) {
	// 装饰器不能改写请求——把画幅或上游模型名吃掉的话，用户按 pro 付费会拿到
	// 别的模型的结果，而没有任何地方报错。
	srv := serveBytes(t, "image/png", pngBytes)
	inner := &fakeInner{url: srv.URL + "/a.png"}
	a := NewStoringAdapter(inner, &fakeStore{})

	req := GenerateRequest{
		Prompt: "cat", Width: 1344, Height: 768,
		UpstreamModel: "flux-pro-1.1", GenerationID: "gen-9",
		// 参考图一并纳入：它的**顺序**对应上游的 input_image、input_image_2…，
		// 装饰器若做了排序或去重，生成结果会变而用户看不出为什么。
		ReferenceImageURLs: []string{"https://img.example.com/ref/1/a.jpg", "https://img.example.com/ref/1/b.jpg"},
	}
	if _, err := a.Generate(context.Background(), req); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// 用 DeepEqual 而不是 !=：GenerateRequest 含切片，不可比较。顺带它也真的
	// 逐个比较了参考图 URL，而 != 只会比较切片头。
	if !reflect.DeepEqual(inner.lastReq, req) {
		t.Errorf("请求被改写了: got %+v, want %+v", inner.lastReq, req)
	}
}

func TestNewStoringAdapterPanicsOnNilInner(t *testing.T) {
	// Registry.Get 的 nil 守卫存在的理由是：nil adapter 会在**行已建、次数已扣**
	// 之后才 panic。包装之后那个守卫失效了——非 nil 的 *StoringAdapter 装着 nil
	// inner，守卫和 ValidateProviders 都放行。所以必须在构造期炸掉：构造只在启动
	// 时跑一次，一次响亮的启动失败正是原守卫想要的结果。
	defer func() {
		if recover() == nil {
			t.Error("NewStoringAdapter(nil, ...) 必须 panic——" +
				"否则 nil inner 会绕过 Registry.Get 的守卫，在扣费之后才 nil 解引用")
		}
	}()
	NewStoringAdapter(nil, &fakeStore{})
}

func TestStoringAdapterExtensionComesFromSniffedType(t *testing.T) {
	// 这个表把 allowedImageTypes 三个条目全部钉住。
	//
	// 为什么值得专门写：之前只有 png 有覆盖，于是"webp 嗅探不出来"这个**错误**
	// 结论可以一路走到删掉 image/webp 条目而没有任何测试变红——而删掉它的后果是
	// 所有 webp 上游图片静默降级、一小时后变成历史里的死链。
	//
	// 同时它证明扩展名与 contentType 都来自**嗅探出的字节**，而不是 URL 后缀或
	// 上游的 Content-Type 头：每个用例都故意把图片挂在 .png 结尾的路径上、并让
	// 服务器谎报 Content-Type 为 image/png。只有 png 用例里两者才恰好一致。
	cases := []struct {
		name    string
		body    []byte
		wantExt string
		wantCT  string
	}{
		{"png", pngBytes, "png", "image/png"},
		{"jpeg", jpegBytes, "jpg", "image/jpeg"},
		{"webp", webpBytes, "webp", "image/webp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := serveBytes(t, "image/png", c.body)
			inner := &fakeInner{url: srv.URL + "/upstream.png"}
			store := &fakeStore{}
			a := NewStoringAdapter(inner, store)

			res, err := a.Generate(context.Background(), GenerateRequest{GenerationID: "gen-x"})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !res.Stored {
				t.Fatalf("%s 是白名单内的类型，必须转存成功并置 Stored", c.name)
			}
			wantKey := "g/gen-x." + c.wantExt
			if store.lastKey != wantKey {
				t.Errorf("key: got %q, want %q（扩展名必须来自嗅探，不是 URL 后缀）",
					store.lastKey, wantKey)
			}
			if store.lastType != c.wantCT {
				t.Errorf("contentType: got %q, want %q（必须来自嗅探，不是上游的头）",
					store.lastType, c.wantCT)
			}
			if res.ImageURL != "https://img.example.com/"+wantKey {
				t.Errorf("ImageURL: got %q", res.ImageURL)
			}
			if string(store.lastBody) != string(c.body) {
				t.Error("上传的字节与下载的不一致")
			}
		})
	}
}
