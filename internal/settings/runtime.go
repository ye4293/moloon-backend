package settings

import (
	"log"
	"strconv"
	"strings"
	"sync/atomic"

	"image-backend/internal/generation"
	"image-backend/internal/storage"
)

// Snapshot 是某一时刻生效的配置值。
//
// 只读：Reload 会整体换掉一个新的 Snapshot 指针，绝不就地修改已发出去的那个。
type Snapshot struct {
	EZLinkAIBaseURL   string
	FluxAPIKey        string
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2PublicBaseURL   string
	AppBaseURL        string

	// SignupBonusCredits 注册赠送的月度次数。0 = 不赠送。
	//
	// 解析失败时取 0 而不是让 Reload 报错：这个快照在热重载路径上构造，而 Reload
	// 失败会让**整个**配置停留在旧版本。在"送 0 次"与"这次保存里其他项（比如刚换的
	// 上游 key）也一起不生效"之间，选前者——前者只是少送额度且有告警。
	// Validate 已在写入时拦过非法值，走到这里的坏值只可能来自手工改库。
	SignupBonusCredits int

	// adapters 与 store 是按上面的值构造好的客户端，随快照一起替换。
	adapters generation.Registry
	// store 单独留一份，而不是只藏在 adapters 里面的 StoringAdapter 中：参考图上传
	// 需要直接往对象存储写（它不经过任何 adapter）。
	store storage.Storage
}

// Storage 当前生效的对象存储。
//
// **必须和 R2PublicBaseURL 从同一个 *Snapshot 上取。** 分两次 rt.snap.Load() 会在
// 热重载的瞬间取到不匹配的一对——用新的 store 写对象、用旧的域名拼 URL，
// 结果是一个 404 的永久链接，而没有任何地方报错。
func (s *Snapshot) Storage() storage.Storage { return s.store }

// StorageEnabled 五项齐全才算配置好（与 config.StorageEnabled 同一判断）。
func (s *Snapshot) StorageEnabled() bool {
	return s.R2Endpoint != "" && s.R2AccessKeyID != "" && s.R2SecretAccessKey != "" &&
		s.R2Bucket != "" && s.R2PublicBaseURL != ""
}

// Runtime 持有当前生效的配置与客户端，支持不重启热替换。
//
// 用 atomic.Pointer 而不是读写锁：读方是每一个请求（生成请求会持有数十秒），
// 加锁读会让写方在改配置时被长请求挡住，而无锁读没有这个问题。写极少发生，
// 每次重建全部客户端的成本可以忽略。
type Runtime struct {
	store *Store
	snap  atomic.Pointer[Snapshot]
}

func NewRuntime(store *Store) (*Runtime, error) {
	rt := &Runtime{store: store}
	if err := rt.Reload(); err != nil {
		return nil, err
	}
	return rt, nil
}

// Reload 从库里重读配置、重建客户端，然后原子替换快照。
func (rt *Runtime) Reload() error {
	vals, err := rt.store.All()
	if err != nil {
		return err
	}
	s := &Snapshot{
		EZLinkAIBaseURL:   valOr(vals, "ezlinkaiBaseUrl", "https://api.ezlinkai.com"),
		FluxAPIKey:        vals["fluxApiKey"],
		R2Endpoint:        vals["r2Endpoint"],
		R2AccessKeyID:     vals["r2AccessKeyId"],
		R2SecretAccessKey: vals["r2SecretAccessKey"],
		R2Bucket:          vals["r2Bucket"],
		R2PublicBaseURL:   vals["r2PublicBaseUrl"],
		AppBaseURL:        vals["appBaseUrl"],
	}
	s.SignupBonusCredits = parseBonus(vals["signupBonusCredits"])
	// store 先建、再交给 buildAdapters 复用同一个实例：两次 buildStorage 会在没配
	// R2 时打两条一样的告警，而更实际的问题是它们各自持有一份 S3 客户端与连接池。
	s.store = buildStorage(s)
	s.adapters = buildAdapters(s, s.store)
	rt.snap.Store(s)
	return nil
}

func (rt *Runtime) Snapshot() *Snapshot           { return rt.snap.Load() }
func (rt *Runtime) Adapters() generation.Registry { return rt.snap.Load().adapters }
func (rt *Runtime) StorageEnabled() bool          { return rt.snap.Load().StorageEnabled() }
func (rt *Runtime) AppBaseURL() string            { return rt.snap.Load().AppBaseURL }

// SignupBonusCredits 当前生效的注册赠送次数。0 = 不赠送。
//
// 做成方法（而不是让调用方拿 Snapshot 自己读）是为了让 handler 能**按请求**取值，
// 于是后台改完立刻生效——与 Adapters / AppBaseURL 同一个约定。
func (rt *Runtime) SignupBonusCredits() int { return rt.snap.Load().SignupBonusCredits }

// buildAdapters 与原先 server.BuildAdapters 同构，只是配置来自快照而非 env。
//
// 每个 adapter 都被 StoringAdapter 包一层：新增 provider 自动获得转存，不依赖
// 谁记得加代码。
//
// store 由调用方传入而不是自己 buildStorage：快照要单独留一份 storage 给参考图上传
// 用（它不经过任何 adapter），两处各建一次会让没配 R2 时打两条重复告警，
// 更实际的问题是各自持有一份 S3 客户端与连接池。
func buildAdapters(s *Snapshot, store storage.Storage) generation.Registry {
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFlux(s), store),
	}
}

func buildFlux(s *Snapshot) generation.Adapter {
	if s.FluxAPIKey == "" {
		// 退化成 stub 而不是拿空 key 去打上游：后者会让每次生成都以"上游认证
		// 失败"收场，而次数已经扣了。
		log.Println("settings: 未配置 fluxApiKey，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	return generation.NewFluxAdapter(s.EZLinkAIBaseURL, s.FluxAPIKey)
}

func buildStorage(s *Snapshot) storage.Storage {
	if !s.StorageEnabled() {
		log.Println("settings: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		s.R2Endpoint, s.R2AccessKeyID, s.R2SecretAccessKey,
		s.R2Bucket, s.R2PublicBaseURL,
	)
}

func valOr(m map[string]string, key, def string) string {
	if v := m[key]; v != "" {
		return v
	}
	return def
}

// parseBonus 解析 signupBonusCredits。**任何解析不出来的值都退化成 0（不赠送）。**
//
// 为什么不返回 error 让 Reload 失败：Reload 失败会让整个配置停留在旧版本，于是同一次
// 保存里其他项（比如刚换的上游 key）也一起不生效。而这一项失败的后果只是少送额度，
// 严重程度完全不同。上限同样在这里兜一道——Validate 是主防线，但手工改库能绕过它，
// 而这一项直接对应真金白银，不该只有一层防护。
func parseBonus(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		log.Printf("settings: signupBonusCredits=%q 不是合法的非负整数，按 0（不赠送）处理", raw)
		return 0
	}
	if n > maxSignupBonusCredits {
		log.Printf("settings: signupBonusCredits=%d 超过上限 %d，按 0（不赠送）处理——"+
			"这一项直接决定送出去多少真钱，宁可不送也不能按一个可疑的大数送", n, maxSignupBonusCredits)
		return 0
	}
	return n
}

// Validate 报告当前生效配置里的问题，供启动时打告警用。
//
// **返回问题列表而不是 error，也不让调用方 Fatal。** 库里的值可能是上一个管理员
// 改坏的，此时拒绝启动等于让一次误操作把服务打死，而正确的行为是带着告警起来、
// 让管理员能登录进去改回来（见设计文档 §2.5）。
func (rt *Runtime) Validate() []string {
	s := rt.Snapshot()
	var problems []string
	for _, kv := range []struct{ k, v string }{
		{"r2PublicBaseUrl", s.R2PublicBaseURL},
		{"appBaseUrl", s.AppBaseURL},
		{"r2Endpoint", s.R2Endpoint},
		{"ezlinkaiBaseUrl", s.EZLinkAIBaseURL},
	} {
		if err := Validate(kv.k, kv.v); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}
