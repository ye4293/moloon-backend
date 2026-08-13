package generation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"image-backend/internal/storage"
)

const (
	// transferTimeout 转存自己的超时。
	//
	// **不继承上游那 5 分钟**：上游花了 4 分 50 秒的话，共用 ctx 只剩 10 秒给
	// 转存、必然降级——而这时候本来是可以再等一会儿的。
	transferTimeout = 60 * time.Second
)

// StoringAdapter 包住任意 Adapter，把上游返回的临时图片 URL 转存到我们自己的
// 存储，换成永久 URL。
//
// 做成装饰器而不是写在 handler 里，是为了顺着项目已有的结构长：Registry +
// Adapter + StubAdapter 已经建立了"provider 行为可替换、可注入假实现"的模式。
// 新增 provider 会自动获得转存，不依赖谁记得加代码；而塞进 handler 则会让
// "转存"只能靠跑完整生成流程来测，而那正是 stub adapter 存在要避免的东西。
type StoringAdapter struct {
	inner  Adapter
	store  storage.Storage
	client *http.Client
}

func NewStoringAdapter(inner Adapter, store storage.Storage) *StoringAdapter {
	// inner 为 nil 时**必须**在这里炸掉。Registry.Get 的 nil 守卫看不穿这一层包装：
	// NewStoringAdapter(nil, ...) 返回的是非 nil 的 *StoringAdapter，守卫放行、
	// ValidateProviders 也放行，然后在 Generate 里 nil 解引用——而那时行已经建了、
	// 次数已经扣了，正是那个守卫当初要避免的时刻。
	if inner == nil {
		panic("NewStoringAdapter: inner adapter 不能为 nil")
	}
	return &StoringAdapter{
		inner:  inner,
		store:  store,
		client: &http.Client{Timeout: transferTimeout},
	}
}

// Generate 先让 inner 生成，再尽力转存。
//
// **转存的任何失败都降级，不返回错误。** 图已经出了、钱已经花在上游了。因为我们
// 自己的存储抖动就判失败并退款，等于把一次成功且已付费的上游调用白扔，用户还得
// 重新排队等 21 秒。降级的最坏后果只是这一张图一小时后失效——比彻底没有强。
func (a *StoringAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	res, err := a.inner.Generate(ctx, req)
	if err != nil {
		return res, err
	}
	if res.ImageURL == "" {
		return res, nil
	}
	// StubAdapter 返回的是前端 public/ 下的相对路径，不是可下载的 URL。
	//
	// **显式跳过，而不是让它走失败降级**：否则本地开发与 e2e 每次生成都会打一条
	// 转存告警，而那条告警正是生产上唯一提示"这张图一小时后会失效"的信号——让它
	// 变成日常噪音，等于把它关掉。
	if !strings.HasPrefix(res.ImageURL, "http://") && !strings.HasPrefix(res.ImageURL, "https://") {
		return res, nil
	}

	url, err := a.transfer(ctx, req.GenerationID, res.ImageURL)
	if err != nil {
		log.Printf("[storing] 转存失败，降级为上游临时链接（约一小时后失效）gen=%s: %v",
			req.GenerationID, err)
		return res, nil
	}
	res.ImageURL = url
	res.Stored = true
	return res, nil
}

func (a *StoringAdapter) transfer(ctx context.Context, genID, srcURL string) (string, error) {
	// WithoutCancel 之后重新计时：见 transferTimeout 的注释。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return "", fmt.Errorf("构造下载请求: %w", err)
	}
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("下载图片: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载图片返回 %d", resp.StatusCode)
	}

	// 多读 1 字节：正好读满上限说明后面还有内容，即超限。
	// 判定交给 SniffImageType（它按 MaxImageBytes 报错），这里只负责不把无限的响应
	// 体读进内存。
	body, err := io.ReadAll(io.LimitReader(resp.Body, storage.MaxImageBytes+1))
	if err != nil {
		return "", fmt.Errorf("读取图片: %w", err)
	}

	// 嗅探内容、不信上游的 Content-Type 头，理由见 storage.SniffImageType 的注释。
	ct, ext, err := storage.SniffImageType(body)
	if err != nil {
		return "", err
	}

	return a.store.Put(ctx, "g/"+genID+"."+ext, ct, body)
}
