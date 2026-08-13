package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/config"
	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/handler"
	"image-backend/internal/model"
)

// setupRefGenRouter 一个既能上传、又能生成的路由，共用同一个假存储。
//
// stub adapter 由 NewRouter 内部按 FluxAPIKey=="" 装配，而我们要断言 handler 传给
// adapter 的入参——所以拿一个 StubAdapter 自己注入，用它的 LastRequest() 观察。
func setupRefGenRouter(t *testing.T) (*gin.Engine, *gorm.DB, *generation.StubAdapter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stub := generation.NewStubAdapter()
	cfg := &config.Config{JWTSecret: "test-secret", ConfigEncryptionKey: testConfigEncryptionKey}
	r := NewRouterWithAdapters(db, cfg, generation.Registry{"flux": stub},
		WithReferenceStore(handler.ReferenceStore{
			Store:      &recordingStore{},
			PublicBase: "https://img.example.com",
		}),
	)
	return r, db, stub
}

// uploadRef 走真实的上传接口拿一个键，而不是自己拼一个。
//
// **刻意不手工构造键**：手工拼的话，上传接口改了键的格式而生成接口没跟上时，
// 这些测试仍然全绿——而线上是坏的。让两个接口在测试里真的对接一次。
func uploadRef(t *testing.T, r *gin.Engine, token string) string {
	t.Helper()
	w := postAuthed(r, token, refPath, `{"image":"`+dataURL("image/png", pngBody)+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("上传参考图失败：%d %s", w.Code, w.Body.String())
	}
	return uploadKeyOf(t, w)
}

// totalCredits 月度 + 加量的总额。被拒的请求两边都不该动。
func totalCredits(a model.CreditAccount) int { return a.MonthlyCredits + a.AddonCredits }

// countGenerations 表里有多少行。校验必须在建行之前，这个数才是 0。
func countGenerations(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.Generation{}).Count(&n).Error; err != nil {
		t.Fatalf("统计 generations: %v", err)
	}
	return n
}

func refGenBody(keys ...string) string {
	quoted := make([]string, 0, len(keys))
	for _, k := range keys {
		quoted = append(quoted, `"`+k+`"`)
	}
	return `{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1","referenceKeys":[` +
		strings.Join(quoted, ",") + `]}`
}

// TestGeneratePassesReferenceURLsToAdapterInOrder 是这一组里最有价值的一条。
//
// 断言 handler **真的**把参考图交给了 adapter，而且**顺序原样**。顺序对应上游的
// input_image、input_image_2…，"第一张是主体、后面是风格参考"的语义来自用户在界面
// 上的排列——任何一层排序或去重都会悄悄改变生成结果，而用户看不出为什么。
//
// 走 StubAdapter.LastRequest()：那个口子存在的全部理由就是这个（见 stub.go 的注释，
// 一个忽略入参的 stub 会让"漏传"完全隐形）。
func TestGeneratePassesReferenceURLsToAdapterInOrder(t *testing.T) {
	r, db, stub := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-order@example.com", "secret12345")
	grantTo(t, db, "ref-order@example.com", 5*modelCredits(t, db, "flux-2-max"))

	// 传三张，故意让它们的键**不是**字典序（b、a、c），这样一旦有人加了排序就会露馅。
	var keys []string
	for range 3 {
		keys = append(keys, uploadRef(t, r, token))
	}

	w := postGenerate(r, token, refGenBody(keys...))
	if w.Code != http.StatusOK {
		t.Fatalf("生成应当 200，得到 %d: %s", w.Code, w.Body.String())
	}

	got, ok := stub.LastRequest()
	if !ok {
		t.Fatal("adapter 没有被调用")
	}
	if len(got.ReferenceImageURLs) != len(keys) {
		t.Fatalf("adapter 收到 %d 张参考图，想要 %d 张——handler 漏传了",
			len(got.ReferenceImageURLs), len(keys))
	}
	for i, k := range keys {
		want := "https://img.example.com/" + k
		if got.ReferenceImageURLs[i] != want {
			t.Errorf("第 %d 张：adapter 收到 %q，想要 %q（顺序被改了？）",
				i+1, got.ReferenceImageURLs[i], want)
		}
	}
}

// TestGenerateWithoutReferencesPassesNone 不传参考图时 adapter 必须收到空。
//
// 这条守的是"新功能不能压垮旧功能"：如果 handler 在没有参考图时传了个
// []string{""}，flux adapter 会把它映射成 input_image=""，而上游按 uri 格式校验
// 会拒掉——所有纯文生图请求一起挂。
func TestGenerateWithoutReferencesPassesNone(t *testing.T) {
	r, db, stub := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-none@example.com", "secret12345")
	grantTo(t, db, "ref-none@example.com", 5*modelCredits(t, db, "flux-2-max"))

	for _, body := range []string{
		`{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1"}`,
		`{"prompt":"quick cat","model":"flux-2-max","aspectRatio":"1:1","referenceKeys":[]}`,
	} {
		w := postGenerate(r, token, body)
		if w.Code != http.StatusOK {
			t.Fatalf("应当 200，得到 %d: %s", w.Code, w.Body.String())
		}
		got, _ := stub.LastRequest()
		if len(got.ReferenceImageURLs) != 0 {
			t.Errorf("不传参考图时 adapter 不该收到任何 URL，得到 %v", got.ReferenceImageURLs)
		}
	}
}

// TestGenerateRejectsAnotherUsersReferenceKey 越权引用。
//
// 桶是公开可读的，键一旦泄露（比如出现在别人的历史里、或者被猜到）就能被直接引用。
// 没有属主校验的话，A 能拿 B 上传的私人照片去做生成。
func TestGenerateRejectsAnotherUsersReferenceKey(t *testing.T) {
	r, db, stub := setupRefGenRouter(t)

	victimToken := registerAndLogin(t, r, "ref-victim@example.com", "secret12345")
	victimKey := uploadRef(t, r, victimToken)

	attackerToken := registerAndLogin(t, r, "ref-attacker@example.com", "secret12345")
	grantTo(t, db, "ref-attacker@example.com", 5*modelCredits(t, db, "flux-2-max"))
	before := totalCredits(balanceOf(t, db, "ref-attacker@example.com"))

	w := postGenerate(r, attackerToken, refGenBody(victimKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("引用别人的参考图应当 400，得到 %d: %s", w.Code, w.Body.String())
	}
	// 校验必须发生在**建行与扣费之前**：拒了却扣了费，用户要走一遍退款；
	// 拒了却建了行，历史里会多一条用户没发起过的失败记录。
	if after := totalCredits(balanceOf(t, db, "ref-attacker@example.com")); after != before {
		t.Errorf("被拒的请求不该扣费：%d -> %d", before, after)
	}
	if n := countGenerations(t, db); n != 0 {
		t.Errorf("被拒的请求不该建 generations 行，建了 %d 条", n)
	}
	if _, called := stub.LastRequest(); called {
		t.Error("被拒的请求不该打到 adapter")
	}
}

func TestGenerateRejectsMalformedReferenceKeys(t *testing.T) {
	r, db, _ := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-bad@example.com", "secret12345")
	uid := grantTo(t, db, "ref-bad@example.com", 20*modelCredits(t, db, "flux-2-max"))
	mine := uploadRef(t, r, token)

	for _, tc := range []struct{ name, key, why string }{
		{"路径穿越", fmt.Sprintf("ref/%d/../../g/someone.png", uid),
			"6 段，被段数检查拦下"},
		{"百分号编码的穿越", fmt.Sprintf("ref/%d/%%2e%%2e/x.png", uid),
			"4 段，被段数检查拦下"},
		{"跨命名空间引用生成结果", "g/some-generation-id.png",
			"那是别人付费产出的成品图，不是参考图"},
		{"段数不对", fmt.Sprintf("ref/%d/sub/dir/x.png", uid), "形状必须恰好三段"},
		{"前缀不对", fmt.Sprintf("xref/%d/x.png", uid), "前缀必须精确等于 ref"},
		{"绝对路径", fmt.Sprintf("/ref/%d/x.png", uid), "开头的斜杠会让键与实际对象对不上"},
		{"空键", "", "空键拼出来的 URL 指向桶根目录"},
		// 下面两条**恰好也是三段**，段数检查够不到，只有规范性检查（path.Clean）
		// 拦得住。它们是 parseReferenceKeys 里那条 Clean 检查存在的**唯一**理由——
		// 少了这两条用例，把 Clean 删掉所有测试仍然全绿（实测过）。
		{"三段但指向上级目录", fmt.Sprintf("ref/%d/..", uid),
			"Clean 之后是 ref，跑出了用户自己的前缀"},
		{"三段但指向本级目录", fmt.Sprintf("ref/%d/.", uid),
			"不是任何一次上传产出过的键，拼出来是个 404 的参考图"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postGenerate(r, token, refGenBody(tc.key))
			if w.Code != http.StatusBadRequest {
				t.Errorf("应当 400（%s），得到 %d: %s", tc.why, w.Code, w.Body.String())
			}
		})
	}

	// 重复的键：不报错的话同一张图会占掉两个槽位，用户会发现"我明明选了不同的图"。
	t.Run("重复的键", func(t *testing.T) {
		w := postGenerate(r, token, refGenBody(mine, mine))
		if w.Code != http.StatusBadRequest {
			t.Errorf("重复的键应当 400，得到 %d: %s", w.Code, w.Body.String())
		}
	})
}

// TestGenerateRejectsTooManyReferenceImages 上限 8。
//
// 与上游一致。在 handler 拦是为了给出一条说得清的 400，而不是等扣完费、
// 请求打到上游之后收一个笼统的 422。
func TestGenerateRejectsTooManyReferenceImages(t *testing.T) {
	r, db, _ := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-many@example.com", "secret12345")
	grantTo(t, db, "ref-many@example.com", 20*modelCredits(t, db, "flux-2-max"))
	before := totalCredits(balanceOf(t, db, "ref-many@example.com"))

	var keys []string
	for range 9 {
		keys = append(keys, uploadRef(t, r, token))
	}

	w := postGenerate(r, token, refGenBody(keys...))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("9 张参考图应当 400（上限 8），得到 %d: %s", w.Code, w.Body.String())
	}
	if after := totalCredits(balanceOf(t, db, "ref-many@example.com")); after != before {
		t.Errorf("被拒的请求不该扣费：%d -> %d", before, after)
	}

	// 边界的另一侧：正好 8 张要通过，否则上限的语义会悄悄变成 7。
	w = postGenerate(r, token, refGenBody(keys[:8]...))
	if w.Code != http.StatusOK {
		t.Errorf("正好 8 张应当通过，得到 %d: %s", w.Code, w.Body.String())
	}
}

// TestGenerateRejectsReferencesForUnsupportedModel 让 supports_image_to_image 这个
// 标记**有效**。
//
// 不校验的话它就退化成纯装饰字段：GET /models 对外声明某模型不支持参考图，
// 而后端照样把参考图发过去——上游要么忽略、要么 422，两种都让用户白付一次钱。
func TestGenerateRejectsReferencesForUnsupportedModel(t *testing.T) {
	r, db, _ := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-unsup@example.com", "secret12345")
	grantTo(t, db, "ref-unsup@example.com", 5*modelCredits(t, db, "flux-2-max"))
	key := uploadRef(t, r, token)

	// 造一个明确不支持图生图的模型。
	noI2I := model.ImageModel{
		ID: "text-only", DisplayName: "Text Only", Provider: "flux",
		UpstreamModel: "flux-2-max", Credits: 1,
		SupportsImageToImage: false, Enabled: true, SortOrder: 99,
	}
	if err := db.Create(&noI2I).Error; err != nil {
		t.Fatalf("造模型: %v", err)
	}
	before := totalCredits(balanceOf(t, db, "ref-unsup@example.com"))

	body := `{"prompt":"quick cat","model":"text-only","aspectRatio":"1:1","referenceKeys":["` + key + `"]}`
	w := postGenerate(r, token, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持图生图的模型带参考图应当 400，得到 %d: %s", w.Code, w.Body.String())
	}
	if after := totalCredits(balanceOf(t, db, "ref-unsup@example.com")); after != before {
		t.Errorf("被拒的请求不该扣费：%d -> %d", before, after)
	}

	// 同一个模型**不带**参考图必须照常工作——守卫只该拦"带图打不支持的模型"。
	w = postGenerate(r, token, `{"prompt":"quick cat","model":"text-only","aspectRatio":"1:1"}`)
	if w.Code != http.StatusOK {
		t.Errorf("不带参考图时该模型应当正常工作，得到 %d: %s", w.Code, w.Body.String())
	}
}

// TestGeneratePersistsAndReturnsReferenceKeys 参考图要落库并在响应里透出。
//
// 落的是**键**不是 URL：R2 公开域名是后台可改的一项，存 URL 的话换域名会让所有
// 历史记录一起变成死链且无法批量修复。
func TestGeneratePersistsAndReturnsReferenceKeys(t *testing.T) {
	r, db, _ := setupRefGenRouter(t)
	token := registerAndLogin(t, r, "ref-persist@example.com", "secret12345")
	grantTo(t, db, "ref-persist@example.com", 5*modelCredits(t, db, "flux-2-max"))
	key := uploadRef(t, r, token)

	w := postGenerate(r, token, refGenBody(key))
	if w.Code != http.StatusOK {
		t.Fatalf("应当 200，得到 %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		ID              string   `json:"id"`
		ReferenceImages []string `json:"referenceImages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	want := "https://img.example.com/" + key
	if len(resp.ReferenceImages) != 1 || resp.ReferenceImages[0] != want {
		t.Errorf("响应里的参考图 %v，想要 [%s]", resp.ReferenceImages, want)
	}

	// 库里存的必须是**键**，不能是 URL。
	var row model.Generation
	if err := db.Where("id = ?", resp.ID).First(&row).Error; err != nil {
		t.Fatalf("读回记录: %v", err)
	}
	if !strings.Contains(row.ReferenceKeys, key) {
		t.Errorf("reference_keys 里应当有键 %q，得到 %q", key, row.ReferenceKeys)
	}
	if strings.Contains(row.ReferenceKeys, "http") {
		t.Errorf("reference_keys 里**不能**存 URL（换公开域名会让历史全变死链），得到 %q",
			row.ReferenceKeys)
	}
}
