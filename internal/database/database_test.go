package database

import (
	"path/filepath"
	"strings"
	"testing"

	"image-backend/internal/model"
)

func TestOpenMigratesUserTable(t *testing.T) {
	db, err := Open("") // 空 DSN → dev 模式 SQLite（临时文件库）
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	u := model.User{Email: "a@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	// 验证 Role/Status 数据库默认值
	var got model.User
	if err := db.First(&got, u.ID).Error; err != nil {
		t.Fatalf("load user failed: %v", err)
	}
	if got.Role != "user" || got.Status != "active" {
		t.Fatalf("expected default role=user status=active, got role=%q status=%q", got.Role, got.Status)
	}
	dup := model.User{Email: "a@b.com", PasswordHash: "y"}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("expected unique constraint error for duplicate email")
	}
}

// 回归测试：dev 模式数据库必须在连接池新开连接后仍可见同一份数据。
// 若用 ":memory:" 内存库（glebarez 驱动不支持跨连接共享），新连接会得到独立空库，
// 出现 "no such table: users"。SetMaxIdleConns(0) 强制每次操作新建连接来复现该场景。
func TestOpenDevModeSurvivesNewConnections(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB failed: %v", err)
	}
	sqlDB.SetMaxIdleConns(0) // 不保留空闲连接，强制后续操作走新连接
	u := model.User{Email: "fresh-conn@b.com", PasswordHash: "x"}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user on fresh connection failed (in-memory db not shared across connections?): %v", err)
	}
}

func TestCreditTxHasBothUniqueIndexes(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	var ddl []string
	// SQLite：从 sqlite_master 读索引定义。这是**实测**而不是相信 GORM 标签——
	// 同一列挂两个 uniqueIndex 的写法能不能生效，只有 DDL 说了算。
	if err := db.Raw(
		"SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name='credit_transactions'").
		Scan(&ddl).Error; err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(ddl, "\n")
	for _, want := range []string{"idx_credit_tx_gen_type", "idx_credit_tx_ext_type"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少唯一索引 %s，实际 DDL:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "UNIQUE") < 2 {
		t.Errorf("两个索引都应是 UNIQUE，实际 DDL:\n%s", joined)
	}
}

func TestSeedPlansIsIdempotentAndDoesNotClobber(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	// 模拟运营调价 + 播种命令回填了 Price ID
	if err := db.Model(&model.Plan{}).Where("id = ?", "pro").
		Updates(map[string]any{"price_usd_cents": 3990, "stripe_price_id": "price_manual"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := seedPlans(db); err != nil {
		t.Fatal(err)
	}
	var p model.Plan
	if err := db.First(&p, "id = ?", "pro").Error; err != nil {
		t.Fatal(err)
	}
	if p.PriceUSDCents != 3990 {
		t.Errorf("播种覆盖了运营调的价：期望 3990，得到 %d", p.PriceUSDCents)
	}
	if p.StripePriceID != "price_manual" {
		t.Errorf("播种抹掉了 StripePriceID，会导致重复创建 Stripe Price：得到 %q", p.StripePriceID)
	}
	var n int64
	db.Model(&model.Plan{}).Count(&n)
	if n != 3 {
		t.Errorf("期望 3 个档位，得到 %d", n)
	}
}

// TestOpenWithFilePathPersistsAcrossReopen 钉住"文件路径的 DATABASE_URL 数据持久"。
//
// 这条不是为了测 SQLite（那是驱动的事），是为了钉住 Open 的**分支判断**：早先的
// 实现把任何非空 databaseURL 都交给 Postgres 驱动，于是本地开发只有"临时库"一个
// 选项——重启即失忆。订阅联调跨越多次重启，在那种库上走不完；而 cmd/seed-stripe
// 在临时库上跑还会把真花钱建出来的 Price ID 扔掉。
func TestOpenWithFilePathPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")
	email := "persist@example.com"

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db1.Create(&model.User{Email: email, Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db1.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows 上不关连接，t.TempDir() 的清理会因为文件仍被占用而让测试失败
	// （断言本身早就过了，失败发生在收尾阶段）。
	t.Cleanup(func() {
		if sqlDB, err := db2.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	var n int64
	if err := db2.Model(&model.User{}).Where("email = ?", email).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("重开同一路径应当读回那行，得到 %d 行——DATABASE_URL 指文件时数据没有持久", n)
	}
}

func TestGenerationHasStoredColumnAndCompositeIndex(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	m := db.Migrator()

	if !m.HasColumn(&model.Generation{}, "stored") {
		t.Error("generations 缺 stored 列——历史页无法区分永久链接与降级后的临时链接")
	}

	// 历史查询是 WHERE user_id = ? AND status <> 'processing'
	// ORDER BY created_at DESC, id DESC。复合索引让过滤和主要排序键走同一棵树；
	// id DESC 只是同一毫秒内的稳定分页 tiebreaker，索引并不覆盖它。现在几十行
	// 看不出差别，攒到几千行时这是"翻页 200ms"与"翻页 3 秒"的差别，而那时候加
	// 索引要锁表。
	if !m.HasIndex(&model.Generation{}, "idx_gen_user_created") {
		t.Error("generations 缺 (user_id, created_at) 复合索引")
	}
	if !m.HasIndex(&model.Generation{}, "idx_gen_gallery") {
		t.Error("generations 缺公开画廊复合索引")
	}
}

// TestOpenDropsStaleUserIDIndex 钉住"旧的单列索引会被清理掉"。
//
// UserID 从 gorm:"index" 改成复合索引成员后，AutoMigrate 只加不删：既有库里的
// idx_generations_user_id 会永远留着，没有查询用它，却要在每次写 generations 时
// 被维护一遍。Open("") 每次都是全新空库，天然看不见这个问题，所以这里必须用
// **文件库**：先手工建出旧索引模拟升级前的库，再重开一次，断言它没了。
func TestOpenDropsStaleUserIDIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-index.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 手工重建升级前的那个索引，名字取自实测的 GORM 默认命名。
	if err := db1.Exec("CREATE INDEX idx_generations_user_id ON generations(user_id)").Error; err != nil {
		t.Fatal(err)
	}
	// 先确认这个测试真的把索引造出来了——否则下面的断言会在"它从来没存在过"的
	// 情况下自动通过，测出个假绿。
	if !db1.Migrator().HasIndex(&model.Generation{}, "idx_generations_user_id") {
		t.Fatal("前置条件没成立：旧索引没造出来，后面就测不到 drop 了")
	}
	sqlDB, err := db1.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db2.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if db2.Migrator().HasIndex(&model.Generation{}, "idx_generations_user_id") {
		t.Error("旧的单列索引 idx_generations_user_id 还在——既有库会一直为它付写入维护成本")
	}
	// 顺带确认清理没有连坐把复合索引也干掉。
	if !db2.Migrator().HasIndex(&model.Generation{}, "idx_gen_user_created") {
		t.Error("复合索引 idx_gen_user_created 不见了")
	}
}

func TestGenerationStoredDefaultsFalse(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// 不显式设 Stored，落库后读回来必须是 false——默认值写错成 true 的后果是
	// 前端对一批一小时后会失效的链接显示"永久有效"。
	g := model.Generation{
		ID: "gen-default-1", UserID: 1, Model: "flux-2-max", Prompt: "p",
		AspectRatio: "1:1", Width: 1024, Height: 1024,
		Status: model.GenStatusSucceeded,
	}
	if err := db.Create(&g).Error; err != nil {
		t.Fatalf("落库: %v", err)
	}
	var got model.Generation
	if err := db.Where("id = ?", "gen-default-1").First(&got).Error; err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Stored {
		t.Error("Stored 默认值应当是 false")
	}
}

// TestOpenEmptyURLIsEphemeralAndIsolated 空 URL 必须仍然是一次性的独立库。
//
// 大量测试依赖这个隔离性（每次 Open("") 拿到自己的空库），所以加了文件路径分支
// 之后要确认没把它破坏掉。IsEphemeral 是 cmd/seed-stripe 那道守卫的依据，一起钉住。
func TestOpenEmptyURLIsEphemeralAndIsolated(t *testing.T) {
	if !IsEphemeral("") {
		t.Error(`IsEphemeral("") 必须为 true——cmd/seed-stripe 靠它拒绝在一次性库上创建真实 Stripe 对象`)
	}
	if IsEphemeral("./local.db") {
		t.Error(`IsEphemeral("./local.db") 必须为 false`)
	}

	dbA, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	if err := dbA.Create(&model.User{Email: "a@example.com", Role: "user", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}
	dbB, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := dbB.Model(&model.User{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("两次 Open(\"\") 应当互相看不见，第二个库里却有 %d 行用户", n)
	}
}

func TestAppSettingTableMigrated(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	m := db.Migrator()
	if !m.HasTable(&model.AppSetting{}) {
		t.Fatal("app_settings 表没有被迁移出来")
	}
	for _, col := range []string{"key", "value", "encrypted", "updated_at"} {
		if !m.HasColumn(&model.AppSetting{}, col) {
			t.Errorf("app_settings 缺列 %s", col)
		}
	}
}

func TestAppSettingEncryptedDefaultsFalse(t *testing.T) {
	// 默认值写错成 true 会让明文项被当成密文去解密，表现是启动时全部配置解不开。
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	s := model.AppSetting{Key: "k1", Value: "v1"}
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("落库: %v", err)
	}
	var got model.AppSetting
	if err := db.Where("key = ?", "k1").First(&got).Error; err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Encrypted {
		t.Error("Encrypted 默认值应当是 false")
	}
}
