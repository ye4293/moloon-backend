package database

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

func TestOpenMigratesGenerationCostFromInteger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	type legacyGeneration struct {
		model.Generation
		UpstreamCost int `gorm:"not null;default:0"`
	}
	if err := db.Table("generations").AutoMigrate(&legacyGeneration{}); err != nil {
		t.Fatal(err)
	}
	var oldType string
	if err := db.Raw(`SELECT type FROM pragma_table_info('generations') WHERE name = 'upstream_cost'`).Scan(&oldType).Error; err != nil {
		t.Fatal(err)
	}
	if oldType != "INTEGER" && oldType != "integer" {
		t.Fatalf("legacy column must be integer, got %s", oldType)
	}
	old := legacyGeneration{
		Generation:   model.Generation{ID: "existing", UserID: 1, Model: "flux-2-max", Prompt: "apple", AspectRatio: "1:1", Width: 1024, Height: 1024, Status: model.GenStatusSucceeded, CreditsSpent: 7},
		UpstreamCost: 7,
	}
	if err := db.Table("generations").Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	migratedDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer migratedDB.Close()
	var got model.Generation
	if err := db.First(&got, "id = ?", "existing").Error; err != nil {
		t.Fatal(err)
	}
	if got.UpstreamCost != 7 || got.CreditsSpent != 7 || got.Prompt != "apple" {
		t.Fatalf("migration changed existing data: %+v", got)
	}
	const cost = 7.25
	if err := db.Model(&got).Update("upstream_cost", cost).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&got, "id = ?", "existing").Error; err != nil {
		t.Fatal(err)
	}
	if got.UpstreamCost != cost || got.CreditsSpent != 7 {
		t.Fatalf("fractional cost not preserved or credits changed: %+v", got)
	}
}
