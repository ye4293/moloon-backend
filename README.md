# image-backend

AI 图像生成订阅平台后端（Go + Gin + GORM + PostgreSQL）。

设计文档：`docs/superpowers/specs/2026-07-27-image-platform-design.md`

## 本地运行

```
docker compose -f docker-compose.dev.yml up -d   # 启动 Postgres
cp .env.example .env                             # 按需修改
go run ./cmd/server                              # 启动服务，默认 :8080
```

不配置 DATABASE_URL 时使用临时文件 SQLite（dev 模式），零配置即可启动。

**本地开发必须带 `-f docker-compose.dev.yml`。** 不带 `-f` 时 compose 用的是根目录
`docker-compose.yml`，那是**生产**配置（拉 CI 镜像 + 外部托管数据库）。这个分工是有意的：
服务器上敲不带参数的 `docker compose up -d` 应该正好部署生产环境，而不是启动一个
弱密码的开发库。

部署见 [deploy/DEPLOY.md](deploy/DEPLOY.md)。

## API（M1 + M3a + M3b）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | /api/v1/health | 健康检查 |
| POST | /api/v1/auth/register | 邮箱注册 {email, password} |
| POST | /api/v1/auth/login | 登录，返回 {token} |
| GET | /api/v1/me | 当前用户（Bearer token），含 `credits.monthly/addon` |
| GET | /api/v1/models | 启用的模型列表（公开），含 `credits` 字段 |
| GET | /api/v1/gallery | 公开画廊，游标分页；仅返回已公开、成功且永久保存的图片 |
| POST | /api/v1/generations | 同步生成一张图（Bearer token） |
| POST | /api/v1/admin/credits | 管理员给指定邮箱发放次数（`role=admin`） |

## 生成链路

`POST /generations` **同步**返回结果——上游 ezlinkai 内部替我们轮询 BFL 并挂住连接直到出图。
实测两次真实生成：**26.3 秒**（16:9，1344×768）与 **23.7 秒**（1:1，1024×1024）。因此不需要
reconcile worker、不需要 `upstream_task_id`、前端不需要轮询。

### 编排顺序不能换

**建 `processing` 行 → 扣费 → 调上游 → 落结果或退款。**

反过来（先扣费再建行）有一个无法补救的窗口：扣费成功后、建行之前进程崩溃，
`credit_transactions` 里留下一条扣费流水，却没有任何 `generations` 行指向它。启动兜底扫描
是靠扫 `processing` 行找回退款的，找不到行就永远退不回来——**用户的钱凭空消失且无人知晓**。

反之先建行、扣费失败（余额不足），留下的是一条 `processing` 行没有扣费流水；扫描调
`Refund` 时找不到扣费流水会静默返回，再把行标成 `failed` 即可，没有损失。

### 上游调用必须用脱离请求的 context

`internal/handler/generations.go` 用 `context.WithTimeout(context.Background(), 5*time.Minute)`，
**刻意不用 `c.Request.Context()`**。生成要 20 多秒，"用户中途关标签页"是常见情况而非边缘情况；
继承请求 context 意味着关页面就取消一次已经付过费的生成，扣了钱丢了图。

**已实测验证**：`curl -m 3` 发起生成后 3 秒强行断开（curl 退出码 28），45 秒后查库，该行是
`succeeded`、有真实图片 URL、`duration_ms=23708`，流水正常。若当初用了请求 context，这一格
会是 `processing` 挂着、钱扣了、图没了。

### 上游两处反直觉的地方（实测所得，别"顺手统一"）

1. **提交响应里的 `polling_url` 装的是最终图片 URL**，不是给你去轮询的地址。
2. **两个端点认证头不一致**：提交用 `x-key`，`get_result` 用 `Authorization: Bearer`。
   统一掉会 401。

`internal/generation/flux.go` 的类型注释里写着这两条，三处设置请求头的地方都有指回注释的提示。
测试对两者都做了**双向断言**（提交必须没有 Authorization、兜底必须没有 x-key）——这种负向断言
才挡得住"统一一下"的重构。

### 无 key 时用 stub，测试才不烧钱

`FLUX_API_KEY` 留空时 `buildFluxAdapter` 返回 stub adapter，出本地占位图，并保留 prompt
关键词触发：`fail`（800ms 后失败并退款）> `slow`（90 秒）> `quick`（200ms）> 默认（15 秒）。

这不是图方便：接真上游后每跑一次端到端测试都会真调 Flux——每次 20 多秒、每次花钱。
前端的 e2e 必须在 stub 模式下跑。

### 启动兜底扫描

`generation.SweepStuck` 回收卡在 `processing` 的行并退款，**必须在开始接收流量之前调用**
（`cmd/server/main.go` 里就在 `NewRouter` 之前）。那时任何 `processing` 行按定义都是上一个
进程遗留的孤儿；服务跑起来之后再扫，会把**正在进行中**的生成误判成孤儿、从活跃用户手里
把次数退回去。幂等由 `(generation_id, type)` 唯一索引保证，所以每次重启都跑是安全的。

### 画幅固定在约 1MP

上游按输出百万像素计价（实测 `cost: 7` ↔ `output_mp: 1`，落库为 `upstream_cost`）。
`1:1 → 1024×1024`、`16:9 → 1344×768`、`9:16 → 768×1344`，都压在约 1MP，让"扣 1 次"对应的
真实成本可预测。不支持的画幅**返回 400 而不是静默纠正成 1:1**——静默纠正会让前端以为自己
传对了，用户拿到另一个比例的图却没有任何提示。

### 配置

代码只读环境变量，**不会自动加载 `.env` 文件**。本地用：

```bash
set -a; . ./.env; set +a
go run ./cmd/server
```

写好 `.env` 却发现不生效，通常就是漏了这一步。

## 次数账本

双余额：`monthly_credits` 随订阅每月重置，`addon_credits` 一次性购买永不过期。
扣费**先扣月度、不足再扣加量包**。

**所有余额变动只能经过 `internal/credit`。** handler 不得直接写 `credit_accounts`——
绕过它就意味着漏流水，而漏了流水的余额无法对账，出问题时只能猜。

### 数据表

- `image_models`：模型配置，GORM 由 `ImageModel` 复数化而来（不是 `models`）。启动时幂等播种 `flux-2-max`，运营可改 `credits` 等字段——用 `FirstOrCreate` 而非 `Save`，每次启动不会覆盖线上调整。
- `credit_accounts`：双余额，`user_id` 为主键（无自增）。
- `credit_transactions`：不可变流水。`monthly_delta` 与 `addon_delta` 分开记，不合并成总数：退款必须按扣费时的拆分还回去，把加量包次数错还成月度次数会在月底重置时凭空蒸发。

### 幂等与安全性

`credit_transactions` 的 `(generation_id, type)` 有复合唯一索引 `idx_credit_tx_gen_type`，
这是退款幂等与"一次生成只扣一次"的**唯一权威**，不靠"先 Count 再 INSERT"：
COUNT 在 READ COMMITTED 下不加锁，两个并发退款会各数到 0 然后都插进去——退两次。
唯一键冲突没有这个窗口。

`generation_id` 是 `*string`（可空）：发放类流水存 `NULL` 而非空串，
因为 SQLite 与 Postgres 都把 NULL 视为互不相等——所有发放记录不会在唯一索引上互相冲突。

`credit.Refund(db, generationID)` **不接收 `userID` 参数**：退给谁由扣费流水说了算。
若接收 `userID`，handler 把 JWT 里的 `userID` 和请求里的 `generationID` 一拼，
拿别人的 generation ID 就能给自己造钱，还会留下无法对账的脏数据。

`credit.Grant` 拒绝负数：它走的是不带 WHERE 守卫的相对 UPDATE，放负数进来能把余额扣成负数。
目前没有管理员冲正/扣减路径——冲正若要做，必须走一条和 `Spend` 同样三层防护的独立路径。

### 扣费的三重保险（缺一不可，见 `internal/credit/ledger.go` 注释）

1. **事务**：余额变动与流水同生共死。
2. **`SELECT ... FOR UPDATE` 行锁**（Postgres 生效；SQLite 靠单连接串行化）。
3. **带条件的 `UPDATE` 并校验 `RowsAffected == 1`**：即使前两层被绕过也不会扣成负数。

`Spend` 针对 **READ COMMITTED**（Postgres 默认）设计。在 REPEATABLE READ 下，
同样的并发交错会抛序列化失败而非重新读取，且本包不实现重试。

### 并发测试需要 Postgres——上线前的硬性门控

`database.Open` dev 模式用临时 SQLite 且 `SetMaxOpenConns(1)`，连接池**串行化**所有并发请求。
在默认配置下并发扣费测试必然通过且什么都没证明。

有两条测试由 `TEST_DATABASE_URL` 门控，环境变量未设置时显式 SKIP 并打印原因（不静默跳过）：

- `TestConcurrentSpendNeverOversells`：验证 30 并发扣费中恰好 10 次成功、余额归零。
- `TestConcurrentRefundRefundsOnce`：**这是唯一覆盖 `Refund` 里重复键回滚分支的测试。**
  该代码路径在 CI 和本地从未执行过。

**在装有 Postgres 的机器上跑通这两条测试是上线前的硬性前置条件，不是可选项。**
它们覆盖的是扣成负数 / 超卖 / 双退——全系统后果最贵的失效模式，且代码路径从未在真实数据库上运行。

```bash
# 需要 Postgres（本机 dev 模式无法运行）
TEST_DATABASE_URL="postgres://imageapp:imageapp@localhost:5432/imageapp?sslmode=disable" \
  go test ./internal/credit/ -run TestConcurrent -v
```

### 给测试账号发次数

注册接口默认不会创建管理员。第一个 admin 用 `BOOTSTRAP_ADMIN_EMAIL` 引导：

```bash
# 带上该环境变量启动服务
BOOTSTRAP_ADMIN_EMAIL=you@example.com go run ./cmd/server
# 然后用该邮箱正常注册——注册完成即是管理员（登录仍然照常走）
curl -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"secret12345"}'
```

两个触发点：**启动时**提权已存在的用户（老库场景），**注册时**提权配置里那个邮箱
（新库场景）。只有后者能在 dev 模式下用——dev 模式每次启动都是一个新的临时 SQLite，
"注册完再重启"会把刚注册的账号一起丢掉。

未配置时两处都完全不执行；只认配置里那一个邮箱；不创建用户；不绕过登录。幂等。

**引导窗口在第一个管理员出现后自动关闭。** `PromoteAdmin` 先数一遍库里有没有 admin，
有就直接返回。这是结构性地关掉一个隐患——注册触发点意味着「谁先注册配置里那个邮箱，谁
就是管理员」，若运维引导完之后忘了取消这个变量（最容易被忘掉的那类事），而那个账号后来
又被删掉，知道配置值的人就能抢注成管理员。有了这个前置条件，就不必依赖任何人记得清理配置。

之后用接口发放（会留流水，比手工 SQL 可追溯）：

```bash
curl -X POST localhost:8080/api/v1/admin/credits \
  -H "Authorization: Bearer <admin-token>" \
  -H 'Content-Type: application/json' \
  -d '{"email":"tester@example.com","monthly":50,"addon":0}'
```

### 已知缺口

- `Spend` 里 `RowsAffected != 1` 把"余额不足"与"账户行在锁外被删了"混为一谈，都返回 `ErrInsufficientCredits`。
- `database.Open` dev 模式每次调用都新建临时 SQLite 文件且从不清理（M1 遗留）。
- 没有管理员冲正 / 扣减路径——`Grant` 只拒绝负数，冲正需独立实现。
- 月度重置无实现，等 Stripe `invoice.paid` 事件（M4）。
- 登录接口无速率限制（M1 遗留），公网部署前必须补。
- `TestConcurrentRefundRefundsOnce` 覆盖的 `Refund` 重复键回滚分支从未在真实数据库执行。
- 内测若继续用临时 SQLite，进程一停账号与余额全没；正式内测前需要 Docker Desktop 或本机 Postgres。
- **上游图片 URL 约 1 小时后失效。** 返回的是 `replicate.delivery` 链接，响应头
  `Cache-Control: public,max-age=3600`，Replicate 官方策略是输出文件 1 小时后清理。R2 转存由
  ezlinkai 侧后续完善，本轮不做——**这意味着 `/history` 在转存完成前基本无意义，历史里全是死链**。
  这条会被当成 bug 反复排查，所以写在这里。
- **`get_result` 兜底路径从未在真实上游执行过。** 两次真实生成的提交都直接返回了 `Ready`，
  所以那段代码只在 `httptest.Server` 上跑过；终态失败状态列表（`Error` / `Content Moderated` /
  `Request Moderated` / `Task not found`）来自文档而非实测。
- 未识别的上游状态会继续轮询（配一条醒目日志）而非快速失败。刻意如此：上游将来新增状态不该
  立刻变成硬失败。代价是那种情况下会空转到 ctx 超时。
- adapter 支持 `Seed`，但 API 没有暴露该字段，handler 恒传 nil。
- 生成接口无速率限制——单个用户可以并发打满上游配额。
- `SweepStuck` **在多实例部署下会互相踩**：每个实例启动都扫全表，可能把另一实例正在进行的生成
  当成孤儿退款。单实例没问题，扩容前必须加实例标识或分布式锁。
- `http.Transport` 未调优（`MaxIdleConnsPerHost` 默认 2），而提交要挂住连接 20 多秒，
  并发生成超过 2 个就会不断新建连接而非复用。
- `generations` 表缺 `(user_id, created_at)` 复合索引，`/history` 会需要；prompt 无长度上限，
  而上游成本随请求增长。

## 开发命令

```
go build ./... && go vet ./...   # 提交前必跑
go test ./...                    # 运行测试（并发测试需 TEST_DATABASE_URL）
```
