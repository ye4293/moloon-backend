package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUpstreamAuth 上游拒绝了我们的凭据（401/403）。它同时也满足 ErrUpstream（仍然
// 要退款），但必须能被单独判定出来。
//
// 为什么值得单独一个哨兵：key 过期是**我们的配置错误**，长得却和"prompt 被安全过滤
// 拒绝"一模一样。混在一起的话，key 一死所有请求都报"上游拒绝了你的 prompt"，没有
// 任何信号能区分"我们的 key 死了导致 100% 失败"与"用户在写触发过滤的 prompt"。
var ErrUpstreamAuth = errors.New("upstream authentication failed")

// FluxAdapter 对接 ezlinkai 的 Flux 端点。
//
// 上游有两处反直觉的地方，都是 2026-07-28 实测所得，不要"顺手统一"掉：
//
//  1. **提交响应里的 `polling_url` 装的是最终图片 URL**，不是给你去轮询的地址。
//     ezlinkai 在内部替我们轮询了 BFL，挂住连接直到出图（实测约 21 秒）。
//  2. **两个端点的认证头不一样**：提交用 `x-key`，`get_result` 用
//     `Authorization: Bearer`。改成统一会 401。
//
// **2026-08-02 补充（网关换成 linkinfra 后读其源码所得）：** 上面第 1 条对
// linkinfra 不成立。两条路径都已覆盖、无需改代码，但改这段之前必须知道：
//
//   - linkinfra 的提交响应**没有 status 字段**（见其
//     `relay/channel/flux/model.go` 的 `FluxResponse`），所以下面那个
//     `Status == Ready` 分支恒假，实际走的是 `getResult`。而它填的
//     `polling_url` 是 `https://api.bfl.ai/v1/get_result?id=...`——真正的轮询
//     端点，不是图片。**万一哪天它开始返回 Ready，那个分支就会把一个 JSON
//     端点当图片存进库**，而且是静默的。
//   - linkinfra 的 `get_result` 会把 `result.sample` 改写成转存后的永久 R2 URL，
//     但**仅当它的 webhook 已经处理完转存**（`StoredSampleURL` 查不到时返回
//     空串）；否则返回 BFL 原始 sample，那是 10 分钟后失效的临时链接。我们自己
//     的 StoringAdapter 正好补掉这个竞态——所以即便上游已经转存过，本项目的
//     `R2_*` 仍然建议配上。
//
// 上游模型名**不在这个结构体里**——它按请求传，见 GenerateRequest.UpstreamModel。
type FluxAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
	// pollInterval 兜底轮询的间隔。做成字段只为让测试能把它调小，生产用默认值。
	pollInterval time.Duration
}

func NewFluxAdapter(baseURL, apiKey string) *FluxAdapter {
	return &FluxAdapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// 不设 Timeout：超时由调用方通过 ctx 控制，那样才能和"脱离请求的
		// context"配合。这里再设一个会变成两个互相打架的期限。
		client:       &http.Client{},
		pollInterval: 3 * time.Second,
	}
}

type fluxSubmitResponse struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	PollingURL string  `json:"polling_url"`
	Cost       float64 `json:"cost"` // 美分；网关可能返回小数，包括 7.000000000000001。
}

type fluxResultResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		Sample string `json:"sample"`
	} `json:"result"`
}

const (
	fluxStatusReady   = "Ready"
	fluxStatusPending = "Pending"
)

// fluxTerminalFailureStatuses 是**答案已知**的失败状态：命中就立刻失败，不再轮询。
//
// 此前唯一的退出条件是"Ready 且有 sample"，其余一律睡 3 秒再轮。于是一个被内容审核
// 拦下的 prompt——第一次轮询就已经拿到 Content Moderated——会被当成"还没好"，空转
// 约 100 次、挂住用户连接整整 5 分钟，最后以超时收场。
//
// 比较用 strings.EqualFold：上游改个大小写不该让我们退化回那条 5 分钟空转。
var fluxTerminalFailureStatuses = []string{
	"Error",
	"Content Moderated",
	"Request Moderated",
	"Task not found",
}

func fluxIsTerminalFailure(status string) bool {
	for _, s := range fluxTerminalFailureStatuses {
		if strings.EqualFold(status, s) {
			return true
		}
	}
	return false
}

// maxFluxReferenceImages 上游接受的参考图数量上限。
//
// BFL 的 Flux2Inputs schema 只开到 input_image_8（宣传口径写的"最多参考 10 张"
// 与 API 不一致，以 API 为准）。handler 也会拦一道——那一道是给用户看的报错，
// 这一道是**最后一道**：超出时宁可在发请求之前失败，也不要提交一个上游必定
// 以 422 拒绝的请求，因为那时次数已经扣了。
const maxFluxReferenceImages = 8

// putReferenceImages 把参考图 URL 映射成上游的 input_image / input_image_2…
//
// 两个必须写对、写错了只会得到一个笼统 422 的地方：
//
//  1. **第一张的字段名是 `input_image`，没有 `_1`。** 循环写成 `input_image_%d`
//     从 1 开始会得到 `input_image_1`，那是个上游不认识的字段——而多数网关对
//     未知字段的处置是**静默忽略**，于是表现成"第一张参考图不起作用"，
//     没有任何错误。
//  2. **一张都没有时，一个 input_image* 键都不能加。** 塞 `"input_image": ""`
//     会被上游按 uri 格式校验拒掉（实测错误是 `Does not match format 'uri'`），
//     于是**所有纯文生图请求突然全挂**——而这个改动本来只该影响图生图。
func putReferenceImages(body map[string]any, urls []string) error {
	if len(urls) == 0 {
		return nil
	}
	if len(urls) > maxFluxReferenceImages {
		return fmt.Errorf("%w: 参考图 %d 张，上游最多接受 %d 张",
			ErrUpstream, len(urls), maxFluxReferenceImages)
	}
	for i, u := range urls {
		key := "input_image"
		if i > 0 {
			key = fmt.Sprintf("input_image_%d", i+1)
		}
		body[key] = u
	}
	return nil
}

func (a *FluxAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if req.UpstreamModel == "" {
		// 我们自己的配置问题（image_models 行缺 upstream_model），但对用户表现为一次
		// 失败的生成，所以仍归到 ErrUpstream，让调用方走同一条退款路径。
		log.Printf("[flux] 请求没带 UpstreamModel，检查 image_models.upstream_model")
		return GenerateResult{}, fmt.Errorf("%w: 未指定上游模型", ErrUpstream)
	}

	body := map[string]any{
		"prompt":           req.Prompt,
		"width":            req.Width,
		"height":           req.Height,
		"output_format":    "jpeg",
		"safety_tolerance": 2,
	}
	// Seed 为 nil 时**不能**塞 0 进去——0 是一个合法的 seed，会让"不指定"变成
	// "每次都用同一个 seed"，用户会发现同样的 prompt 永远出同一张图。
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}
	if err := putReferenceImages(body, req.ReferenceImageURLs); err != nil {
		return GenerateResult{}, err
	}

	sub, err := a.submit(ctx, req.UpstreamModel, body)
	if err != nil {
		return GenerateResult{}, err
	}

	if strings.EqualFold(sub.Status, fluxStatusReady) && sub.PollingURL != "" {
		return GenerateResult{
			ImageURL:     sub.PollingURL,
			UpstreamID:   sub.ID,
			UpstreamCost: sub.Cost,
		}, nil
	}

	// 未就绪：走兜底查询。这条路径在实测中没出现过（提交总是直接返回 Ready），
	// 但一旦 ezlinkai 内部超时先返回，没有它就是扣了次数拿不到图且无从补救。
	imageURL, err := a.getResult(ctx, sub.ID)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ImageURL: imageURL, UpstreamID: sub.ID, UpstreamCost: sub.Cost}, nil
}

func (a *FluxAdapter) submit(ctx context.Context, upstreamModel string, body map[string]any) (fluxSubmitResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	// upstreamModel 来自数据库（运维可填），转义后再拼进路径。
	endpoint := fmt.Sprintf("%s/flux/v1/%s", a.baseURL, url.PathEscape(upstreamModel))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-key", a.apiKey) // 提交端点用 x-key（见类型注释）

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// 双 %w：既满足 ErrUpstream 的分类契约，又保留 context.Canceled 之类的原错误
		// 能被 errors.Is 判定。
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交请求失败: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 读取响应失败: %w", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fluxSubmitResponse{}, httpError("提交", resp.StatusCode, payload)
	}
	var out fluxSubmitResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		// 原始响应体只进日志：它会经 handler 落进 generations.error 并直达用户浏览器，
		// 而网关信封可能带着我们的账号、额度、内部主机名或 key 前缀。
		log.Printf("[flux] 提交响应解析失败: %v，原始响应: %s", err, truncate(string(payload), 300))
		return fluxSubmitResponse{}, fmt.Errorf("%w: 上游响应格式无法解析", ErrUpstream)
	}
	// 网关额度不足时会返回 **HTTP 200 加错误信封**（例如
	// {"error":{"message":"insufficient user quota"}}）。json.Unmarshal 会成功——未知
	// 字段被忽略——于是 id/status/polling_url 全空。不挡的话就是拿着空 id 去轮询
	// `?id=` 五分钟然后 500。这是真 key 上线后最可能遇到的第一个生产事故。
	if out.ID == "" && out.PollingURL == "" {
		log.Printf("[flux] 提交返回 %d 但既无 id 也无图片 URL，原始响应: %s",
			resp.StatusCode, truncate(string(payload), 300))
		return fluxSubmitResponse{}, fmt.Errorf("%w: 上游未返回任务 id", ErrUpstream)
	}
	return out, nil
}

// getResult 轮询兜底端点直到就绪、终态失败或 ctx 到期。
func (a *FluxAdapter) getResult(ctx context.Context, id string) (string, error) {
	// id 来自上游响应，转义后再拼进查询串。
	endpoint := fmt.Sprintf("%s/flux/v1/get_result?id=%s", a.baseURL, url.QueryEscape(id))
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		// 兜底端点用 Bearer，与提交端点相反（见类型注释）。
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("%w: 兜底查询失败: %w", ErrUpstream, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("%w: 读取兜底响应失败: %w", ErrUpstream, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", httpError("兜底查询", resp.StatusCode, payload)
		}
		var out fluxResultResponse
		if err := json.Unmarshal(payload, &out); err != nil {
			log.Printf("[flux] 兜底响应解析失败: %v，原始响应: %s", err, truncate(string(payload), 300))
			return "", fmt.Errorf("%w: 上游响应格式无法解析", ErrUpstream)
		}
		if strings.EqualFold(out.Status, fluxStatusReady) {
			if out.Result.Sample != "" {
				return out.Result.Sample, nil
			}
			// Ready 意味着上游认为这事完了，sample 不会再出现。送回循环就是在一个已知
			// 答案上空转到超时，所以当成"上游返回了畸形的成功响应"。
			log.Printf("[flux] id=%s 状态 Ready 但没有 result.sample，原始响应: %s",
				id, truncate(string(payload), 300))
			return "", fmt.Errorf("%w: 上游报告就绪但没有返回图片", ErrUpstream)
		}
		if fluxIsTerminalFailure(out.Status) {
			log.Printf("[flux] id=%s 终态失败，状态 %q", id, out.Status)
			return "", fmt.Errorf("%w: 上游任务失败（状态 %s）", ErrUpstream, out.Status)
		}
		if !strings.EqualFold(out.Status, fluxStatusPending) {
			// 没见过的状态：可能是上游新增的终态。只留痕不改行为——继续轮询由 ctx 兜底，
			// 但日志里要能看出"我们不认识这个状态"，否则它表现为一次莫名的超时。
			log.Printf("[flux] id=%s 出现未识别状态 %q，继续轮询", id, out.Status)
		}

		select {
		case <-time.After(a.pollInterval):
		case <-ctx.Done():
			// 双 %w：分类归一到 ErrUpstream（此前这里返回**裸的**
			// context.DeadlineExceeded，与提交阶段分类不一致），同时保留原错误可判定。
			return "", fmt.Errorf("%w: 等待上游结果超时: %w", ErrUpstream, ctx.Err())
		}
	}
}

// httpError 把非 2xx 归一成错误，并把原始响应体记进**日志**而不是错误文案。
//
// 一个字符串没法同时服务两个不兼容的受众：运维要原始响应体来诊断，终端用户不该看到
// 我们的网关信封（handler 会把错误文案落进 generations.error 并回给浏览器）。边界划在
// adapter 这里，而不是指望 handler 去过滤。
func httpError(stage string, statusCode int, payload []byte) error {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		// 喊出来：key 过期会让 100% 的请求失败，混在普通上游错误里要烧掉一个下午。
		log.Printf("[flux] **认证失败**（%s返回 %d）——检查 FLUX_API_KEY 是否过期或额度用尽。原始响应: %s",
			stage, statusCode, truncate(string(payload), 300))
		return fmt.Errorf("%w: %w: %s返回 %d", ErrUpstream, ErrUpstreamAuth, stage, statusCode)
	}
	log.Printf("[flux] %s返回 %d，原始响应: %s", stage, statusCode, truncate(string(payload), 300))
	return fmt.Errorf("%w: %s返回 %d", ErrUpstream, stage, statusCode)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
