package server

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/billing"
	"image-backend/internal/config"
	"image-backend/internal/generation"
	"image-backend/internal/handler"
	"image-backend/internal/middleware"
	"image-backend/internal/settings"
	"image-backend/internal/storage"
)

// RouterOption 覆盖路由内部构造的依赖。
//
// 存在的理由只有一个：webhook 的业务规则（按 Price 反查档位、交叉校验 user_id）
// 依赖一次"拉取订阅"的上游调用，而那几条规则失守时不会有任何征兆——必须能在不
// 联网的情况下测。生产代码不传任何 option。
type RouterOption func(*routerDeps)

type routerDeps struct {
	subs billing.SubscriptionFetcher
	// refs 非 nil 时替换参考图上传用的存储。
	refs *handler.ReferenceStore
}

// WithSubscriptionFetcher 替换 webhook 拉取订阅的实现（测试注入假实现用）。
func WithSubscriptionFetcher(f billing.SubscriptionFetcher) RouterOption {
	return func(d *routerDeps) { d.subs = f }
}

// WithReferenceStore 替换参考图上传用的存储（测试注入假 R2 用）。
//
// 需要它是因为真实的 R2 路径无法在测试里走通，而"上传接口"最要紧的几条不变量
// （拒绝非图片时**没有写入**、扩展名取自嗅探而非声明值）恰恰只能通过观察存储
// 收到了什么来断言。同 storage/r2_test.go 用 httptest.Server 假扮 R2 的思路，
// 只是这里连 HTTP 那一层也不需要。
func WithReferenceStore(refs handler.ReferenceStore) RouterOption {
	return func(d *routerDeps) { d.refs = &refs }
}

// NewRouter 用**自建**的 settings.Store / Runtime 组装一个完整路由。
//
// **这不是生产入口，生产走 NewRouterWithRuntime。** 生产必须在开始接流量之前先
// 播种、跑启动校验、跑 ValidateProviders，那些只有 main.go 亲自持有 Store 与
// Runtime 才做得到。本函数存在的意义是让测试一行拿到完整路由。
//
// cfg.ConfigEncryptionKey 非法时**直接 panic，绝不退化成全零密钥**。
// 退化的后果是所有 secret 用一把人人都猜得到的密钥"加密"入库——任何拿到库的人
// 都能解开，而日志里看不出任何异常。静默的安全降级比起不来严重得多：起不来会
// 立刻被发现并修好，降级会一路服务到某天被拖库才暴露。
func NewRouter(db *gorm.DB, cfg *config.Config, opts ...RouterOption) *gin.Engine {
	key, err := settings.ParseKey(cfg.ConfigEncryptionKey)
	if err != nil {
		panic(fmt.Sprintf(
			"server.NewRouter: CONFIG_ENCRYPTION_KEY 非法: %v\n"+
				"刻意不退化成全零密钥——那等于所有 secret 明文入库且毫无告警。\n"+
				"生成密钥：openssl rand -base64 32\n"+
				"另外：生产入口是 NewRouterWithRuntime，不是本函数。", err))
	}
	st := settings.NewStore(db, key)
	rt, err := settings.NewRuntime(st)
	if err != nil {
		// Store.All() 在空库上不会失败，走到这里说明库真的读不了。
		panic(fmt.Sprintf("server.NewRouter: 初始化 settings.Runtime 失败: %v", err))
	}
	return newRouterFull(db, cfg, rt.Adapters, &handler.AdminSettingsHandler{Store: st, Runtime: rt}, rt.AppBaseURL(), rt.SignupBonusCredits, runtimeRefStore(rt), opts...)
}

// NewRouterWithAdapters 让调用方自己提供 Registry。
//
// **签名不可改**：现有大量 server 测试靠它注入自己的 stub adapter
// （TestGeneratePassesUpstreamModelAndDimensions、TestGeneratePassesGenerationIDToAdapter、
// TestGenerateStoresImageAndReportsStoredTrue），改签名会一次性打断它们。
func NewRouterWithAdapters(db *gorm.DB, cfg *config.Config, adapters generation.Registry, opts ...RouterOption) *gin.Engine {
	// 把固定 Registry 包进闭包：GenerationsHandler.Adapters 现在是
	// func() generation.Registry，每个请求取一次，于是注入语义与原先完全一致。
	//
	// 这条路径**不注册**后台设置接口：它服务的是"自带 adapter 的隔离测试"，
	// 那些测试不需要设置页，也没有 Store 可给。
	return newRouterFull(db, cfg, func() generation.Registry { return adapters }, nil, cfg.AppBaseURL, nil, configRefStore(cfg), opts...)
}

// NewRouterWithRuntime 是 cmd/server/main.go 用的**生产入口**。
//
// main.go 先建好 Store 与 Runtime，才能在接流量之前完成播种、启动校验与
// ValidateProviders。同时收 Store 与 Runtime 是为了不必把 Runtime.store
// 这个私有字段暴露出去、也不必为它加一个 Store() getter。
func NewRouterWithRuntime(db *gorm.DB, cfg *config.Config, st *settings.Store, rt *settings.Runtime, opts ...RouterOption) *gin.Engine {
	return newRouterFull(db, cfg, rt.Adapters, &handler.AdminSettingsHandler{Store: st, Runtime: rt}, rt.AppBaseURL(), rt.SignupBonusCredits, runtimeRefStore(rt), opts...)
}

// newRouterFull 是所有路由注册的唯一实现处，三个公开入口都汇聚到这里。
//
// getAdapters 在 GenerationsHandler 内部**按请求**调用，于是热重载后的配置
// 立刻生效。adminSettings 为 nil 时不注册设置接口（测试注入路径）。
func newRouterFull(
	db *gorm.DB,
	cfg *config.Config,
	getAdapters func() generation.Registry,
	adminSettings *handler.AdminSettingsHandler,
	appBaseURL string,
	// getSignupBonus 按请求返回当前生效的注册赠送次数，nil 表示不赠送。
	// 与 getAdapters 同一个约定：做成 getter 而非取值，后台改完立刻生效。
	getSignupBonus func() int,
	// getRefStore 按请求返回参考图上传用的存储与公开域名。同上的 getter 约定。
	// **存储与公开域名必须成对返回**，理由见 handler.ReferenceStore 的注释。
	getRefStore func() handler.ReferenceStore,
	opts ...RouterOption,
) *gin.Engine {
	r := gin.Default()

	// CORS 来源列表非法时**直接 panic**，与本文件 NewRouter 对 ConfigEncryptionKey
	// 的处理同理：校验只放在 main.go 的话，任何别的构造入口（第二个 cmd、e2e
	// harness、把本包当库用）都会拿到中间件却拿不到"拒绝启动"的保护，于是
	// CORS_ALLOWED_ORIGINS=moloom.ai 这类永远匹配不上的值会静默生效。
	if err := cfg.ValidateCORS(); err != nil {
		panic(fmt.Sprintf("server: %v", err))
	}

	// **关掉尾斜杠重定向。** gin 默认 true，而它是唯一一条全局中间件跑不到的路径：
	// handleHTTPRequest 在 tsr 命中时直接 redirectTrailingSlash(c) 并 return，从不给
	// c.handlers 赋值、从不 c.Next()。后果是前端 base URL 多一个尾斜杠时，预检正常
	// 回 204（预检走 NoRoute，中间件跑得到），浏览器于是放行真实请求，而真实请求
	// 拿到的 301/307 **不带任何 CORS 头**，被浏览器拦掉——同时 Logger 也被跳过，
	// 服务端日志里连一行都没有。curl -L 则完全正常。
	//
	// 关掉之后尾斜杠路径落到 NoRoute，回一个**带 CORS 头**的 404：浏览器读得到，
	// 前端能立刻看出是路径写错了。GET 的 301 还会被浏览器长期缓存，那更难排查。
	r.RedirectTrailingSlash = false

	// 设置可信代理，决定 c.ClientIP() 的取值——按 IP 限流的正确性完全建立在它上面。
	//
	// **危险的是"根本不调这个函数"**：gin.New() 的默认值是
	// trustedProxies = ["0.0.0.0/0", "::/0"]（gin.go:225），即信任所有来源，于是任何人
	// 加一个 X-Forwarded-For 头就换一个新的限流桶。启动日志里那句
	// "You trusted all proxies, this is NOT safe" 说的就是这个默认值。
	//
	// 传 nil 反而是安全的一侧：parseTrustedProxies 会把 trustedCIDRs 置为 nil，
	// 而 isTrustedProxy 在 trustedCIDRs == nil 时直接 return false（gin.go:470），
	// 也就是"谁都不信"，ClientIP 退化成直连来源。所以配置为空时不需要特殊处理，
	// 但仍显式传空切片让意图写在代码里而不是依赖这个不太直观的等价关系。
	proxies := cfg.AllowedProxies()
	if proxies == nil {
		proxies = []string{}
	}
	if err := r.SetTrustedProxies(proxies); err != nil {
		// 这里 panic 而不是忽略：忽略掉的话 engine 保持上一次的值（首次即默认的
		// "信任所有"），限流被一个请求头绕过而日志里没有任何痕迹。
		// config.ValidateTrustedProxies 已在启动时拦过一道，走到这里说明有它没覆盖的
		// 形式，属于必须暴露的情况。
		panic(fmt.Sprintf("server: TRUSTED_PROXIES 无法应用: %v", err))
	}
	log.Printf("proxies: 信任 %v 的 X-Forwarded-For（ClientIP 取值依赖它；"+
		"限流被触发时日志会打出 ClientIP，那里若是内网地址说明这项配窄了）", proxies)

	// CORS 必须在注册任何路由**之前** Use：gin 的路由组在注册时会用 combineHandlers
	// 对当前中间件链做一次**快照**，所以任何在这一行之上注册的路由会永久拿不到 CORS。
	// （预检那条路不依赖顺序：Use 会重建 allNoRoute。）
	//
	// 三个导出入口（NewRouter / NewRouterWithAdapters / NewRouterWithRuntime）
	// 全部汇聚到本函数，所以这一行覆盖全部路由。
	r.Use(middleware.CORS(cfg.AllowedOrigins()))
	api := r.Group("/api/v1")
	api.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 认证接口单独一组，叠加限流与 Content-Type 收紧。
	//
	// 为什么只给这两条：它们是**未认证**且有副作用的接口（建用户、烧 bcrypt CPU、
	// 将来还要发注册赠送额度），是唯一能被匿名脚本反复打的入口。已认证的接口有
	// token 门槛，滥用成本完全不同，套上限流反而会误伤正常的批量操作。
	//
	// JSONOnly 无条件挂；限流按配置挂（零值 Config 视为关闭，测试走这条路，
	// 否则一个测试函数里注册几次就会以「注册返回 429」的形式失败）。
	authHandler := &handler.AuthHandler{DB: db, Cfg: cfg, SignupBonus: getSignupBonus}
	authMiddleware := []gin.HandlerFunc{middleware.JSONOnly()}
	if cfg.RateLimitEnabled() {
		authMiddleware = append(authMiddleware,
			middleware.RateLimit(cfg.RateLimitRPS, cfg.RateLimitBurst))
	}
	authGroup := api.Group("", authMiddleware...)
	authGroup.POST("/auth/register", authHandler.Register)
	authGroup.POST("/auth/login", authHandler.Login)

	modelsHandler := &handler.ModelsHandler{DB: db}
	api.GET("/models", modelsHandler.Get)

	// 公开：定价页在未登录时就要能看到档位。
	plansHandler := &handler.PlansHandler{DB: db}
	api.GET("/plans", plansHandler.List)

	// billing.New 在没有 STRIPE_SECRET_KEY 时返回 nil，handler 据此回 503。
	billingClient := billing.New(cfg.StripeSecretKey, appBaseURL)

	// webhook 拉取订阅用的实现。**必须先判 billingClient 是否为 nil**：把一个 nil
	// 的 *billing.Client 直接赋给接口字段，接口本身是非 nil 的，调用时会 panic 在
	// webhook 里，而那会让 Stripe 无限重投同一个事件。
	deps := routerDeps{}
	if billingClient != nil {
		deps.subs = billingClient
	}
	for _, opt := range opts {
		opt(&deps)
	}

	// 公开：Stripe 不带我们的 cookie，所以 webhook **不能**挂在 authed 组下
	// （挂上去线上所有事件都会 401 被丢弃）。安全性由 handler 内的验签保证。
	api.POST("/webhooks/stripe", (&handler.StripeWebhookHandler{
		DB: db, Secret: cfg.StripeWebhookSecret, Subs: deps.subs,
	}).Handle)

	meHandler := &handler.MeHandler{DB: db}
	authed := api.Group("", middleware.Auth(cfg.JWTSecret), middleware.RequireActiveUser(db))
	authed.GET("/me", meHandler.Get)

	// 参考图用的存储与公开域名。测试可以用 WithReferenceStore 换成假的。
	// 生成与上传两个 handler **必须共用同一个 refStore**：一个用它写对象、另一个
	// 用它把键拼回 URL，两边取到不同的配置会写进去一个地址、读出来另一个。
	refStore := getRefStore
	if deps.refs != nil {
		refs := *deps.refs
		refStore = func() handler.ReferenceStore { return refs }
	}

	generationsHandler := &handler.GenerationsHandler{DB: db, Adapters: getAdapters, Refs: refStore}
	authed.POST("/generations", generationsHandler.Create)
	authed.GET("/generations", generationsHandler.List)

	// 参考图上传单独叠一层 JSONOnly。
	//
	// **这一条是必需的，不是洁癖。** 它是全站唯一接收用户提供的**文件内容**的入口，
	// 而如果它收 multipart，跨域页面就能直接打它：multipart/form-data 与 text/plain
	// 一样属于 CORS **简单请求**类型、**不触发预检**（见 middleware/jsononly.go 的
	// 完整论证）。收 application/json 并挂上 JSONOnly 之后，跨域请求必须先过预检，
	// 而预检会被 CORS 白名单拦下。
	//
	// authed 组整体不挂 JSONOnly（那会一次性改变所有已认证接口的契约），所以这里
	// 按路由挂——`authed.POST(path, mw, handler)` 只影响这一条。
	uploadsHandler := &handler.UploadsHandler{Refs: refStore}
	authed.POST("/uploads/reference", middleware.JSONOnly(), uploadsHandler.Create)

	billingHandler := &handler.BillingHandler{DB: db, Billing: billingClient}
	authed.POST("/billing/subscribe", billingHandler.Subscribe)
	authed.POST("/billing/portal", billingHandler.Portal)

	adminHandler := &handler.AdminHandler{DB: db}
	admin := authed.Group("/admin", middleware.RequireAdmin(db))
	admin.POST("/credits", adminHandler.GrantCredits)

	// 用户管理。**只有查与改，没有删**：User 没有软删除也没有级联，硬删会留下
	// 孤儿 credit_accounts / generations / subscriptions，而 Stripe 那边的 customer
	// 还在扣款。封禁（status=banned）由 RequireActiveUser 立即生效，且可逆。
	adminUsers := &handler.AdminUsersHandler{DB: db}
	admin.GET("/users", adminUsers.List)
	admin.PATCH("/users/:id", adminUsers.Patch)

	// 模型配置：改扣费、上下架、接新模型都不必改代码发版。
	// getAdapters() 在构造时取一次快照：provider 集合是静态的，即使 Reload
	// 改了 API key，provider 名字不会变，校验结果不变。
	adminModels := &handler.AdminModelsHandler{DB: db, Adapters: getAdapters()}
	admin.GET("/models", adminModels.List)
	admin.POST("/models", adminModels.Create)
	admin.PATCH("/models/:id", adminModels.Patch)

	// 档位配置：调月度次数、上下架。与公开的 GET /plans 相反，后台要能看到
	// stripe_price_id 和已下架的档位。价格与 Price ID 不可改（见 handler 注释）。
	adminPlans := &handler.AdminPlansHandler{DB: db}
	admin.GET("/plans", adminPlans.List)
	admin.PATCH("/plans/:id", adminPlans.Patch)

	// 后台设置：仅当调用方提供了 AdminSettingsHandler（生产路径 & NewRouter）。
	// NewRouterWithAdapters（测试注入路径）不注册这两条路由，保持原有测试行为。
	if adminSettings != nil {
		admin.GET("/settings", adminSettings.Get)
		admin.PATCH("/settings", adminSettings.Patch)
	}

	return r
}

// BuildAdapters 构造 provider → adapter 注册表。
//
// 每个 adapter 都被 StoringAdapter 包一层：上游返回的图片 URL 约一小时后失效，
// 不转存的话历史记录里全是死链，而用户为那些图付过费。包在这里而不是各 adapter
// 内部，新增 provider 就自动获得转存，不依赖谁记得加代码。
//
// 导出是为了让 cmd/server/main.go 能先建好、校验完 provider 再交给路由，以及
// 让 TestBuildAdaptersWrapsEveryProviderInStoringAdapter 直接测试这条路径。
// 注意：生产路径（NewRouter / NewRouterWithRuntime）现在走 Runtime.buildAdapters，
// 该路径由 TestRuntimeAdaptersAlwaysWrappedInStoringAdapter 覆盖。
func BuildAdapters(cfg *config.Config) generation.Registry {
	store := buildStorage(cfg)
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFluxAdapter(cfg), store),
	}
}

// runtimeRefStore 生产路径的参考图存储 getter（配置来自后台设置）。
//
// **一次 rt.Snapshot() 取齐两样东西。** 写成 rt.Storage() + rt.Snapshot().R2PublicBaseURL
// 会有两次 atomic Load，中间若发生热重载就拿到不匹配的一对——用新的存储写对象、
// 用旧的域名拼 URL，结果是一个 404 的永久链接，而没有任何地方报错。
func runtimeRefStore(rt *settings.Runtime) func() handler.ReferenceStore {
	return func() handler.ReferenceStore {
		s := rt.Snapshot()
		return handler.ReferenceStore{Store: s.Storage(), PublicBase: s.R2PublicBaseURL}
	}
}

// configRefStore 从 env 取的版本，供 NewRouterWithAdapters 这条测试路径用。
func configRefStore(cfg *config.Config) func() handler.ReferenceStore {
	store := buildStorage(cfg)
	return func() handler.ReferenceStore {
		return handler.ReferenceStore{Store: store, PublicBase: cfg.R2PublicBaseURL}
	}
}

// buildStorage 在没配 R2 时退化成 NoopStorage。
//
// 与 FluxAPIKey 为空退化成 stub、StripeSecretKey 为空禁用计费是同一个约定：
// 本地开发不必凑齐所有外部依赖也能跑完整流程。
func buildStorage(cfg *config.Config) storage.Storage {
	if !cfg.StorageEnabled() {
		log.Println("storage: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		cfg.R2Endpoint, cfg.R2AccessKeyID, cfg.R2SecretAccessKey,
		cfg.R2Bucket, cfg.R2PublicBaseURL,
	)
}

// buildFluxAdapter 在没有配置 key 时退化成 stub。
//
// 这不是"方便"，是必需：接真上游后端到端测试每跑一次都真调 Flux——每次约 21
// 秒、每次花钱。stub 保留 fail/slow/quick 关键词，让测试既快又免费。
func buildFluxAdapter(cfg *config.Config) generation.Adapter {
	if cfg.FluxAPIKey == "" {
		log.Println("generation: FLUX_API_KEY 未配置，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	// 上游模型名不在这里传：它按请求从 image_models.upstream_model 取，这样同一个
	// provider 下的多个模型才不会被静默提交到同一个上游路径。
	return generation.NewFluxAdapter(cfg.EZLinkAIBaseURL, cfg.FluxAPIKey)
}
