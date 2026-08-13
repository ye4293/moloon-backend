package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"image-backend/internal/middleware"
	"image-backend/internal/model"
)

const (
	defaultListLimit = 20
	maxListLimit     = 50
)

// encodeCursor 把一行的排序键编成不透明游标。
//
// **不透明**（base64）是为了以后能换实现而不破坏已经拿着游标的客户端。带上 id
// 而不只是时间戳：created_at 可能完全相同（同一秒内的两次生成），只按时间戳翻页
// 会漏行或重复。
func encodeCursor(g model.Generation) string {
	raw := g.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + g.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 不是合法 base64: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("cursor 结构不对")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 时间戳不合法: %w", err)
	}
	return ts, parts[1], nil
}

// List 返回当前用户的生成历史，游标分页，倒序。
//
// 这个接口是"用户付了钱能拿回自己的图"的唯一读路径——在它存在之前，客户端一旦
// 丢掉 POST /generations 的响应（关标签页、断网、刷新），图片就永久不可达，而
// 次数已经扣了。
func (h *GenerationsHandler) List(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)

	// limit 越界**钳制而不报错**：客户端传个 0 是常见的 off-by-one，回 400 只是
	// 把问题推回去，而这里没有任何需要调用方修正的语义。
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

	// 不返回 processing：它要么很快转终态，要么会被启动兜底扫描回收，露出来只会
	// 让用户看到一个永远转圈的格子。
	q := h.DB.Where("user_id = ? AND status <> ?", userID, model.GenStatusProcessing)

	// **默认也不返回 failed。** 历史页是用来看自己出过什么图的，失败的灰格子混在
	// 里面是噪音。但它们**必须仍然查得到**——生成失败时用户第一反应是担心被扣钱，
	// 而那条记录上的"未扣次数"正是唯一能打消这个疑虑的东西（见前端 history-card
	// 的注释）。所以是默认隐藏 + 一个开关，不是彻底删掉。
	//
	// 过滤放在 WHERE 里而不是让前端筛：客户端过滤会出现"点了加载更多但一条没多"
	// （整页都是 failed 时），而空态判定还要和游标打架。
	//
	// **只认字面量 "true"**。`includeFailed=1` / `=yes` / `=on` 一律当 false ——
	// 宽松解析会让一个随手拼错的查询串悄悄打开它，而"悄悄多出一堆失败记录"看起来
	// 像是系统出了问题。
	if c.Query("includeFailed") != "true" {
		q = q.Where("status <> ?", model.GenStatusFailed)
	}

	if raw := c.Query("cursor"); raw != "" {
		ts, id, err := decodeCursor(raw)
		if err != nil {
			// **不能静默当第一页处理**：那会让翻页在游标格式变更后无声地从头开始，
			// 用户以为图丢了。
			c.JSON(http.StatusBadRequest,
				gin.H{"code": errCodeBadRequest, "message": "invalid cursor"})
			return
		}
		// 展开写而不用行值比较元组 (created_at, id) < (?, ?)：SQLite 与 Postgres
		// 对行值比较的支持不一致，而本项目两边都要跑。
		//
		// 已实测（EXPLAIN QUERY PLAN）：这个条件**不会**让查询丢掉
		// idx_gen_user_created，但它是在索引扫描上做后置过滤而不是区间 seek，
		// 所以第 N 页要先扫过约 (N-1) × page_size 行索引项。第 100 页每页 20 行
		// 约 1980 行，亚毫秒级，且不比同深度的 offset 分页差。**这不是缺陷，
		// 不需要优化**——别把它当 bug 去"修"。
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", ts, ts, id)
	}

	// 多取一行来判断有没有下一页——比额外跑一次 COUNT 便宜，也不会像 COUNT 那样
	// 因为并发插入而出现"说有下一页、翻过去是空的"。
	var rows []model.Generation
	if err := q.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		log.Printf("[generations] 历史查询失败 user=%d: %v", userID, err)
		c.JSON(http.StatusInternalServerError,
			gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// nextCursor 用 any 而不是 string：没有下一页时要序列化成 JSON null，空字符串
	// 会让前端把"没有更多"与"游标是空串"混在一起。
	var next any
	if len(rows) > limit {
		rows = rows[:limit]
		next = encodeCursor(rows[len(rows)-1])
	}

	// 参考图 URL 由键加当前配置的公开域名拼出来（见 model.Generation.ReferenceKeys
	// 为什么存键不存 URL）。取一次，整页复用。
	publicBase := h.Refs().PublicBase

	out := make([]gin.H, 0, len(rows))
	for _, g := range rows {
		out = append(out, toGenerationResponse(g, publicBase))
	}
	c.JSON(http.StatusOK, gin.H{"generations": out, "nextCursor": next})
}
