package handler

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/model"
)

// GalleryHandler serves only durable images that their owners chose to publish.
// Its response is deliberately separate from the authenticated history response:
// reference image keys and billing details must never appear in a public payload.
type GalleryHandler struct {
	DB *gorm.DB
}

type galleryImage struct {
	ID          string `json:"id"`
	ImageURL    string `json:"imageUrl"`
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	AspectRatio string `json:"aspectRatio"`
	CreatedAt   string `json:"createdAt"`
}

func (h *GalleryHandler) List(c *gin.Context) {
	limit := defaultListLimit
	if raw := c.Query("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	q := h.DB.Where("is_public = ? AND status = ? AND stored = ? AND image_url <> ?",
		true, model.GenStatusSucceeded, true, "")
	if raw := c.Query("cursor"); raw != "" {
		ts, id, err := decodeCursor(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid cursor"})
			return
		}
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", ts, ts, id)
	}

	var rows []model.Generation
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		log.Printf("[gallery] 查询失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[len(rows)-1])
	}
	images := make([]galleryImage, 0, len(rows))
	for _, row := range rows {
		images = append(images, galleryImage{
			ID: row.ID, ImageURL: row.ImageURL, Model: row.Model,
			Prompt: row.Prompt, AspectRatio: row.AspectRatio,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"images": images, "nextCursor": next})
}
