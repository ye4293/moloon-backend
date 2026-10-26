package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"image-backend/internal/model"
)

func getGallery(t *testing.T, r *gin.Engine, query string) struct {
	Images     []map[string]any `json:"images"`
	NextCursor *string          `json:"nextCursor"`
} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gallery"+query, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("gallery status = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Images     []map[string]any `json:"images"`
		NextCursor *string          `json:"nextCursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGalleryOnlyExposesPublicStoredSuccesses(t *testing.T) {
	r, db := setupRouterWithDB(t)
	registerAndLogin(t, r, "gallery@example.com", "secret12345")
	var user model.User
	db.Where("email = ?", "gallery@example.com").First(&user)
	base := time.Now().UTC().Truncate(time.Second)
	rows := []model.Generation{}
	for i, id := range []string{"visible", "private", "temporary", "failed", "no-url"} {
		g := genRow(id, user.ID, base.Add(time.Duration(i)*time.Second), model.GenStatusSucceeded, true)
		g.IsPublic = true
		g.ReferenceKeys = `["ref/secret-photo.jpg"]`
		switch id {
		case "private":
			g.IsPublic = false
		case "temporary":
			g.Stored = false
		case "failed":
			g.Status = model.GenStatusFailed
		case "no-url":
			g.ImageURL = ""
		}
		rows = append(rows, g)
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	page := getGallery(t, r, "") // deliberately no authorization header
	if got := ids(page.Images); !reflect.DeepEqual(got, []string{"visible"}) {
		t.Fatalf("public images = %v", got)
	}
	if page.NextCursor != nil {
		t.Fatalf("unexpected cursor: %v", *page.NextCursor)
	}
	image := page.Images[0]
	wantFields := []string{"aspectRatio", "createdAt", "id", "imageUrl", "model", "prompt"}
	gotFields := make([]string, 0, len(image))
	for key := range image {
		gotFields = append(gotFields, key)
	}
	sort.Strings(gotFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("公开字段 = %v, want %v", gotFields, wantFields)
	}
}

func TestGalleryCursorPaginatesEqualTimestamps(t *testing.T) {
	r, db := setupRouterWithDB(t)
	registerAndLogin(t, r, "gallery-pages@example.com", "secret12345")
	var user model.User
	db.Where("email = ?", "gallery-pages@example.com").First(&user)
	createdAt := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"a", "b", "c"} {
		g := genRow(id, user.ID, createdAt, model.GenStatusSucceeded, true)
		g.IsPublic = true
		if err := db.Create(&g).Error; err != nil {
			t.Fatal(err)
		}
	}
	first := getGallery(t, r, "?limit=2")
	if got := ids(first.Images); !reflect.DeepEqual(got, []string{"c", "b"}) || first.NextCursor == nil {
		t.Fatalf("first page = %v, cursor = %v", got, first.NextCursor)
	}
	second := getGallery(t, r, "?limit=2&cursor="+*first.NextCursor)
	if got := ids(second.Images); !reflect.DeepEqual(got, []string{"a"}) || second.NextCursor != nil {
		t.Fatalf("second page = %v, cursor = %v", got, second.NextCursor)
	}
}
