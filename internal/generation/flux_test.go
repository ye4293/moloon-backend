package generation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestFluxAdapter 造一个轮询间隔极短的 adapter：生产间隔是 3 秒，测试里等 3 秒
// 只为看它循环一次不值得。
func newTestFluxAdapter(baseURL, apiKey string) *FluxAdapter {
	a := NewFluxAdapter(baseURL, apiKey)
	a.pollInterval = 5 * time.Millisecond
	return a
}

func TestFluxAcceptsNumericCosts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		want  float64
	}{
		{"integer", `"cost":7,`, 7},
		{"gateway_float", `"cost":7.000000000000001,`, 7.000000000000001},
		{"fractional_cent", `"cost":7.25,`, 7.25},
		{"null", `"cost":null,`, 0},
		{"omitted", "", 0},
	} {
		for _, async := range []bool{false, true} {
			mode := "ready"
			if async {
				mode = "processing"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var submits, polls int
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						submits++
						if async {
							_, _ = io.WriteString(w, `{`+tc.field+`"id":"task","status":"processing","polling_url":"https://api.bfl.ai/v1/get_result?id=task"}`)
						} else {
							_, _ = io.WriteString(w, `{`+tc.field+`"id":"task","status":"Ready","polling_url":"https://cdn.example/image.jpg"}`)
						}
						return
					}
					polls++
					if r.URL.Path != "/flux/v1/get_result" || r.URL.Query().Get("id") != "task" {
						t.Errorf("unexpected polling URL: %s", r.URL)
					}
					_, _ = io.WriteString(w, `{"id":"task","status":"Ready","result":{"sample":"https://cdn.example/image.jpg"}}`)
				}))
				defer srv.Close()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				res, err := newTestFluxAdapter(srv.URL, "test-key").Generate(ctx, GenerateRequest{
					Prompt: "apple", Width: 1024, Height: 1024, UpstreamModel: "flux-2-max",
				})
				if err != nil {
					t.Fatal(err)
				}
				if res.UpstreamCost != tc.want || res.UpstreamID != "task" || res.ImageURL != "https://cdn.example/image.jpg" {
					t.Fatalf("unexpected result: %+v", res)
				}
				wantPolls := 0
				if async {
					wantPolls = 1
				}
				if submits != 1 || polls != wantPolls {
					t.Fatalf("submits=%d, polls=%d; want 1, %d", submits, polls, wantPolls)
				}
			})
		}
	}
}

func TestFluxSubmitReturnsImageFromPollingURL(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-key")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cost":7,"id":"abc123","input_mp":0,"output_mp":1,
			"polling_url":"https://cdn.example/img.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "test-key")
	seed := 42
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "a cat", Width: 1024, Height: 1024, Seed: &seed, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if gotPath != "/flux/v1/flux-2-max" {
		t.Fatalf("提交路径错误: %s", gotPath)
	}
	// 提交端点用 x-key，**不是** Authorization。这是上游的不一致，实测所得。
	if gotKey != "test-key" {
		t.Fatalf("提交应当用 x-key 头: got %q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("提交不该带 Authorization: got %q", gotAuth)
	}
	if gotBody["prompt"] != "a cat" || gotBody["width"] != float64(1024) {
		t.Fatalf("请求体错误: %+v", gotBody)
	}
	if gotBody["seed"] != float64(42) {
		t.Fatalf("seed 未透传: %+v", gotBody)
	}
	// output_format 与 safety_tolerance 是实测确定的上游契约。没有断言的话，谁改掉
	// 都不会有人发现，直到用户拿到 png 或被安全过滤挡下。
	if gotBody["output_format"] != "jpeg" {
		t.Fatalf("output_format 应当是 jpeg: %+v", gotBody)
	}
	if gotBody["safety_tolerance"] != float64(2) {
		t.Fatalf("safety_tolerance 应当是 2: %+v", gotBody)
	}

	// polling_url 装的是最终图片 URL，不是轮询地址。
	if res.ImageURL != "https://cdn.example/img.png" {
		t.Fatalf("图片 URL 错误: %s", res.ImageURL)
	}
	if res.UpstreamID != "abc123" || res.UpstreamCost != 7 {
		t.Fatalf("上游元数据错误: %+v", res)
	}
}

func TestFluxOmitsSeedWhenNil(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	if _, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := gotBody["seed"]; present {
		t.Fatalf("Seed 为 nil 时不该出现在请求体里: %+v", gotBody)
	}
}

// I1：上游模型名来自请求而不是构造参数。同一个 adapter 实例必须能提交到不同路径，
// 否则 image_models 里第二行同 provider 的模型会被静默提交到前一行的上游模型。
func TestFluxRoutesByRequestUpstreamModel(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	for _, m := range []string{"flux-2-max", "flux-2-pro"} {
		if _, err := a.Generate(context.Background(), GenerateRequest{
			Prompt: "p", Width: 1, Height: 1, UpstreamModel: m,
		}); err != nil {
			t.Fatalf("generate %s: %v", m, err)
		}
	}
	if len(paths) != 2 || paths[0] != "/flux/v1/flux-2-max" || paths[1] != "/flux/v1/flux-2-pro" {
		t.Fatalf("同一实例应当按请求路由到不同上游模型: %v", paths)
	}
}

func TestFluxRejectsMissingUpstreamModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("不该发出任何请求")
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("缺少上游模型名应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxFallsBackToGetResultWhenNotReady(t *testing.T) {
	var getResultAuth, getResultKey, getResultID string
	var submits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			getResultAuth = r.Header.Get("Authorization")
			getResultKey = r.Header.Get("x-key")
			getResultID = r.URL.Query().Get("id")
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/late.png"},"status":"Ready"}`))
			return
		}
		atomic.AddInt32(&submits, 1)
		// 提交返回未就绪，且没有给出图片 URL——必须走兜底查询。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "test-key")
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("应当通过兜底查询拿到结果: %v", err)
	}
	if res.ImageURL != "https://cdn.example/late.png" {
		t.Fatalf("兜底结果错误: %s", res.ImageURL)
	}
	if getResultID != "abc" {
		t.Fatalf("兜底查询未带 id: %q", getResultID)
	}
	// 兜底端点用 Authorization: Bearer，**不是** x-key。与提交端点相反。
	if getResultAuth != "Bearer test-key" {
		t.Fatalf("兜底应当用 Bearer: got %q", getResultAuth)
	}
	if getResultKey != "" {
		t.Fatalf("兜底不该带 x-key: got %q", getResultKey)
	}
	// 走兜底不等于可以重复提交——重复提交等于重复计费。
	if n := atomic.LoadInt32(&submits); n != 1 {
		t.Fatalf("提交应当只发生一次: got %d", n)
	}
}

// I5：轮询循环此前从未以"循环"的形式跑过——桩第一次调用就返回 Ready。这条盯着第
// 二次迭代真的会发生。
func TestFluxPollsUntilReady(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			if atomic.AddInt32(&polls, 1) < 3 {
				_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/loop.png"},"status":"Ready"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err != nil {
		t.Fatalf("应当轮询到就绪: %v", err)
	}
	if res.ImageURL != "https://cdn.example/loop.png" {
		t.Fatalf("结果错误: %s", res.ImageURL)
	}
	if n := atomic.LoadInt32(&polls); n != 3 {
		t.Fatalf("应当轮询 3 次才就绪: got %d", n)
	}
}

// C1：终态失败状态在第一次轮询就已知，不能当成"还没好"接着轮到超时。
func TestFluxTerminalFailureStatusesFailFast(t *testing.T) {
	statuses := []string{"Error", "Content Moderated", "Request Moderated", "Task not found",
		// 大小写不该影响判定：上游改个大小写就退化成 5 分钟空转是不可接受的。
		"content moderated", "error"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			var polls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/flux/v1/get_result" {
					atomic.AddInt32(&polls, 1)
					_, _ = w.Write([]byte(`{"id":"abc","status":"` + status + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
			}))
			defer srv.Close()

			a := NewFluxAdapter(srv.URL, "k")
			// 一旦它选择"再轮一次"，这条测试会挂住而不是悄悄变慢。
			a.pollInterval = time.Hour
			start := time.Now()
			_, err := a.Generate(context.Background(), GenerateRequest{
				Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
			})
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("终态失败应当归一成 ErrUpstream: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("终态失败应当立即返回，实际 %v", elapsed)
			}
			if n := atomic.LoadInt32(&polls); n != 1 {
				t.Fatalf("终态失败不该继续轮询: got %d 次", n)
			}
		})
	}
}

// C1：Ready 但 sample 为空同样是终态——Ready 意味着上游认为这事完了，sample 不会
// 再出现。当成"还没好"就是在一个已知答案上空转到超时。
func TestFluxReadyWithoutSampleFailsFast(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			atomic.AddInt32(&polls, 1)
			_, _ = w.Write([]byte(`{"id":"abc","status":"Ready","result":{"sample":""}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k")
	a.pollInterval = time.Hour
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("Ready 但无 sample 应当归一成 ErrUpstream: %v", err)
	}
	if n := atomic.LoadInt32(&polls); n != 1 {
		t.Fatalf("不该继续轮询: got %d 次", n)
	}
}

// C3：网关额度不足时会返回 HTTP 200 加错误信封，json.Unmarshal 会成功（未知字段被
// 忽略），于是 id/status/polling_url 全空。拿着空 id 去轮询五分钟是真 key 上线后最
// 可能遇到的第一个生产事故。
func TestFluxSubmit200WithoutIDFailsFast(t *testing.T) {
	var polled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			polled = true
			_, _ = w.Write([]byte(`{"id":"","status":"Pending"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient user quota"}}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k")
	a.pollInterval = time.Hour
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("200 但拿不到 id 应当归一成 ErrUpstream: %v", err)
	}
	if polled {
		t.Fatal("不该拿着空 id 去轮询")
	}
	// I3：网关信封可能带我们的额度/账号信息，不能进入用户可见的错误文案。
	if strings.Contains(err.Error(), "insufficient user quota") {
		t.Fatalf("上游原始响应体不该出现在错误里: %v", err)
	}
}

func TestFluxUpstreamErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"prompt rejected by safety filter"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("上游 4xx 应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxGetResultNon2xxIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"gateway exploded"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("兜底查询非 2xx 应当归一成 ErrUpstream: %v", err)
	}
}

// I3：上游原始响应体只进日志，不进用户可见的错误文案。今天是无害的 detail，明天可
// 能是带着我们账号、额度、内部主机名或 key 前缀的网关信封。
func TestFluxDoesNotLeakUpstreamBodyIntoError(t *testing.T) {
	const secret = "internal-host-10.0.0.7 key=sk-abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"` + secret + `"}`))
	}))
	defer srv.Close()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("应当失败")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "10.0.0.7") {
		t.Fatalf("上游响应体泄漏进错误文案（会直达用户浏览器）: %v", err)
	}
}

// I4：401/403 是**我们的**配置问题（key 过期），必须能与"prompt 被安全过滤拒绝"区
// 分开。退款动作两者都对，但混在一起会烧掉一个下午——key 一死，所有请求都报"上游
// 拒绝了你的 prompt"，没有任何信号指向真正的原因。
func TestFluxAuthFailureIsDistinguishable(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"detail":"invalid api key"}`))
		}))

		a := newTestFluxAdapter(srv.URL, "k")
		_, err := a.Generate(context.Background(), GenerateRequest{
			Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
		})
		srv.Close()

		if !errors.Is(err, ErrUpstream) {
			t.Fatalf("%d 仍应当归一成 ErrUpstream（仍要退款）: %v", code, err)
		}
		if !errors.Is(err, ErrUpstreamAuth) {
			t.Fatalf("%d 应当能与普通上游错误区分开: %v", code, err)
		}
	}
}

// C2：提交阶段的 ctx 取消也要包成 ErrUpstream，且保留原错误可判定。
func TestFluxHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(ctx, GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("已取消的 ctx 应当立即返回错误")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("提交阶段的 ctx 取消也要归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
}

// C2：轮询阶段的超时此前返回**裸的** context.DeadlineExceeded——同一个用户可见的失
// 败，分类却因发生阶段而不同。这条把两个阶段钉成一致。
func TestFluxPollingTimeoutIsWrappedAsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 永远 Pending：只能靠 ctx 到期退出。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(ctx, GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
	})
	if err == nil {
		t.Fatal("ctx 到期应当返回错误")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("轮询阶段超时也要归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
}

// TestFluxMapsReferenceImagesToNumberedFields 参考图到 input_image / input_image_2…
// 的映射。
//
// **第一张的字段名没有 `_1`。** 循环写成 `input_image_%d` 从 1 开始会得到
// `input_image_1`——那是上游不认识的字段，而多数网关对未知字段的处置是**静默忽略**，
// 于是表现成『第一张参考图不起作用』，没有任何错误信息。这条断言存在的全部理由
// 就是钉住这个编号起点。
func TestFluxMapsReferenceImagesToNumberedFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	urls := []string{
		"https://img.example.com/ref/1/a.jpg",
		"https://img.example.com/ref/1/b.jpg",
		"https://img.example.com/ref/1/c.jpg",
	}
	a := newTestFluxAdapter(srv.URL, "k")
	if _, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
		ReferenceImageURLs: urls,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	// 第一张是裸的 input_image，第二三张才带序号，且序号从 2 起。
	for field, want := range map[string]string{
		"input_image":   urls[0],
		"input_image_2": urls[1],
		"input_image_3": urls[2],
	} {
		got, ok := gotBody[field]
		if !ok {
			t.Errorf("请求体缺少 %s。若你把循环写成从 1 开始，第一张会变成上游不认识的 "+
				"input_image_1，而网关会静默忽略它——表现是『第一张参考图不起作用』", field)
			continue
		}
		if got != want {
			t.Errorf("%s = %v，想要 %v", field, got, want)
		}
	}
	// **绝不能出现 input_image_1。**
	if _, bad := gotBody["input_image_1"]; bad {
		t.Errorf("请求体里出现了 input_image_1——上游没有这个字段，第一张图会被静默丢弃: %+v", gotBody)
	}
	// 也不能凭空多出没传的槽位。
	if _, bad := gotBody["input_image_4"]; bad {
		t.Errorf("只传了 3 张却出现了 input_image_4: %+v", gotBody)
	}
}

// TestFluxOmitsReferenceFieldsWhenNone 没有参考图时，一个 input_image* 键都不能有。
//
// 塞 `"input_image": ""` 会被上游按 uri 格式校验拒掉（实测错误是
// `Does not match format 'uri'`），于是**所有纯文生图请求突然全挂**——而这个改动
// 本来只该影响图生图。这条守的是"新功能不能压垮旧功能"。
func TestFluxOmitsReferenceFieldsWhenNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		urls []string
	}{
		{"nil", nil},
		{"空切片", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
			}))
			defer srv.Close()

			a := newTestFluxAdapter(srv.URL, "k")
			if _, err := a.Generate(context.Background(), GenerateRequest{
				Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
				ReferenceImageURLs: tc.urls,
			}); err != nil {
				t.Fatalf("generate: %v", err)
			}
			for k := range gotBody {
				if strings.HasPrefix(k, "input_image") {
					t.Errorf("没有参考图时不该出现 %s（空串会被上游按 uri 校验拒掉，"+
						"于是所有纯文生图请求一起挂）: %+v", k, gotBody)
				}
			}
		})
	}
}

// TestFluxRejectsTooManyReferenceImagesBeforeSending 超过上游上限时**不发请求**。
//
// handler 已经拦过一道（那一道给用户一条说得清的 400），这一道是最后一道：
// 走到这里说明 handler 那道漏了，而此时**次数已经扣了**。发出去只会换回一个
// 笼统的 422，然后走退款——不如在发之前就失败，少一次上游往返。
func TestFluxRejectsTooManyReferenceImagesBeforeSending(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	urls := make([]string, maxFluxReferenceImages+1)
	for i := range urls {
		urls[i] = "https://img.example.com/ref/1/x.jpg"
	}
	a := newTestFluxAdapter(srv.URL, "k")
	_, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "p", Width: 1, Height: 1, UpstreamModel: "flux-2-max",
		ReferenceImageURLs: urls,
	})
	if err == nil {
		t.Fatal("超过上限应当报错")
	}
	// 必须包 ErrUpstream：调用方靠它决定要不要退款。
	if !errors.Is(err, ErrUpstream) {
		t.Errorf("错误必须包 ErrUpstream（调用方靠它判断退款），得到 %v", err)
	}
	if calls != 0 {
		t.Errorf("超限时不该发请求，发了 %d 次", calls)
	}
}
