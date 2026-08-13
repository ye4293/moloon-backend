package generation

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"image-backend/internal/model"
)

// ErrUpstream 上游生成失败（模型拒绝、超时、返回错误状态）。调用方据此退款并把
// generation 标成 failed。与"我们自己写错了"区分开：后者应当是 500。
//
// **实现方注意**：这个契约要求所有"会让用户看到失败"的错误都包上 ErrUpstream，
// 超时与 ctx 取消也算。分类不能按阶段浮动——同一个用户可见的失败，发生在提交阶段
// 还是轮询阶段，归类必须一致，否则调用方按 errors.Is 判断时行为会随时机变化。
var ErrUpstream = errors.New("upstream generation failed")

// GenerateRequest 只包含**我们自己的**领域概念。
//
// 各 provider 如何把它翻译成自家请求体、又如何从自家响应里挖出图片 URL，全部
// 关在各自的 adapter 内。**不要**为了"统一"往这里加 provider 专属字段——产品
// 要求兼容各官方 API 的功能，那种通用参数结构在第三家一定漏。
type GenerateRequest struct {
	Prompt string
	Width  int
	Height int
	Seed   *int // nil 表示不指定

	// UpstreamModel 是上游认识的模型标识，由调用方从 image_models 行里取。
	//
	// 它**放在请求里而不是 adapter 的构造参数里**：Registry 只按 provider 索引，
	// 每个 provider 只有一个 adapter 实例。把上游模型名焊死在实例上的话，表里
	// 一旦出现第二行同 provider 的模型（比如 flux-2-pro），请求会被静默提交到
	// 前一个模型的路径——用户按 pro 付费、拿到 max 的结果，而 generations 行记
	// 的是 pro，没有任何地方报错。
	//
	// 这不是设计文档 §3 拒绝的"通用参数结构"：上游模型标识是**路由信息**，设计
	// 文档正是为了让它按行变化才把它存进 image_models 表的。
	UpstreamModel string

	// GenerationID 是我们自己的 generations 行 id，用来拼转存后的对象 key
	// （g/<id>.<ext>）。
	//
	// 这不是 §3 拒绝的"provider 专属字段"——它是**我们的**领域标识，而且正因为
	// key 由它确定性推导，generations 表才不需要额外存一列 storage_key（两份
	// 可能不一致的真相）。
	GenerationID string

	// ReferenceImageURLs 用户上传的参考图，按顺序传给上游做图生图 / 编辑。
	// 空表示纯文生图。
	//
	// **顺序有意义**：上游把它们映射成 input_image、input_image_2… 这类带序号的
	// 字段，而"第一张是主体、后面几张是风格参考"这种语义由用户在界面上的排列决定。
	// 任何一层做排序或去重都会悄悄改变生成结果，而用户看不出为什么。
	//
	// 这同样不是 §3 拒绝的"provider 专属字段"——"用户提供的参考图"是**我们的**
	// 领域概念（产品需求就是它），与 Prompt / Width / Height 同级；各家 API 怎么
	// 表达它（BFL 是 input_image_N，别家可能是数组或 multipart）留在各自 adapter 里。
	// 判据与 §3 一致：这里放"用户想要什么"，adapter 里放"某一家怎么表达它"。
	//
	// **传 URL 而不是键或字节。** 键需要拼公开域名，那要求 adapter 认识对象存储的
	// 配置——而 adapter 的职责里没有存储。字节则会让请求体在内存里多背几 MB，
	// 且上游本来就要求 URI（实测裸 base64 会被拒）。拼 URL 的活留在 handler。
	ReferenceImageURLs []string
}

type GenerateResult struct {
	ImageURL string
	// UpstreamID 上游任务 id，落库便于事后对账。
	UpstreamID string
	// UpstreamCost 上游报告的成本，与我们扣的次数是两回事，落库便于核算毛利。
	UpstreamCost int
	// Stored ImageURL 是否已经指向我们自己的存储。
	//
	// false 表示它还是上游的临时链接，约一小时后失效。落到
	// generations.stored，历史接口透给前端做提示。
	Stored bool
}

type Adapter interface {
	// Generate 同步返回结果。实现内部负责兜底查询与错误归一化。
	//
	// ctx **必须**是脱离 HTTP 请求生命周期的 context——客户端断开不应该取消
	// 一次已经付过费的生成。
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

// Registry 按 provider 名字选 adapter。
type Registry map[string]Adapter

func (r Registry) Get(provider string) (Adapter, error) {
	a, ok := r[provider]
	if !ok {
		return nil, fmt.Errorf("没有注册 provider %q 的 adapter", provider)
	}
	// 注册成 nil 与没注册一样致命，但不挡的话返回的是 (nil, nil)：调用方拿着 nil
	// 接口去调 Generate 直接 panic，而那时行已经建了、次数已经扣了。
	if a == nil {
		return nil, fmt.Errorf("provider %q 注册的 adapter 是 nil", provider)
	}
	return a, nil
}

// ValidateProviders 校验 image_models 里所有启用行的 provider 都能在 Registry 里
// 解析出来，返回人类可读的问题列表（空列表表示没问题）。
//
// **要在开始接流量之前调用。** 否则 provider 字段打错一个字母，得等第一个选中该
// 模型的用户以 500 的形式替我们发现——那是最贵的一种发现方式。
func ValidateProviders(db *gorm.DB, r Registry) []string {
	var models []model.ImageModel
	if err := db.Where("enabled = ?", true).Find(&models).Error; err != nil {
		return []string{fmt.Sprintf("无法读取 image_models 校验 provider: %v", err)}
	}
	var problems []string
	for _, m := range models {
		if _, err := r.Get(m.Provider); err != nil {
			problems = append(problems, fmt.Sprintf("模型 %q 的 provider 无法解析: %v", m.ID, err))
		}
	}
	return problems
}
