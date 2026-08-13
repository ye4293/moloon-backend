package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"image-backend/internal/generation"
	"image-backend/internal/model"
)

// AdminModelsHandler 让运营在后台改模型扣费、上下架模型、接新模型，不必改代码发版。
//
// 需要 Adapters 是为了校验 provider：建一个没注册 adapter 的 provider，模型在列表里
// 看着完全正常，用户一点生成就 500（GenerationsHandler 里 h.Adapters.Get 失败）。
// 把校验放在写入时，故障点就离改动最近。
type AdminModelsHandler struct {
	DB       *gorm.DB
	Adapters generation.Registry
}

// minModelCredits 每次生成至少扣 1 次。
//
// credit.Spend 拒绝 cost <= 0，所以 credits=0 **不是**"免费模型"，而是"该模型每次
// 生成都返回错误"。写入时拦住，否则故障离配置改动很远，没人会联想到是那次改动。
const minModelCredits = 1

// adminModelResponse 后台视角的模型行。
//
// 与公开的 modelResponse 刻意不同：后台要看到 provider / upstreamModel / enabled /
// sortOrder 才能判断配置对不对，公开接口不该暴露上游细节。
type adminModelResponse struct {
	ID                   string `json:"id"`
	DisplayName          string `json:"displayName"`
	Provider             string `json:"provider"`
	UpstreamModel        string `json:"upstreamModel"`
	Credits              int    `json:"credits"`
	SupportsImageToImage bool   `json:"supportsImageToImage"`
	Enabled              bool   `json:"enabled"`
	SortOrder            int    `json:"sortOrder"`
}

func toAdminModelResponse(m model.ImageModel) adminModelResponse {
	return adminModelResponse{
		ID:                   m.ID,
		DisplayName:          m.DisplayName,
		Provider:             m.Provider,
		UpstreamModel:        m.UpstreamModel,
		Credits:              m.Credits,
		SupportsImageToImage: m.SupportsImageToImage,
		Enabled:              m.Enabled,
		SortOrder:            m.SortOrder,
	}
}

// List 返回**所有**模型，包括已下架的。
//
// 公开的 GET /models 只返回 enabled=true。后台若也过滤，下架一个模型之后就再也
// 找不到它、无法重新上架——运营只剩手改数据库这条路。
func (h *AdminModelsHandler) List(c *gin.Context) {
	var rows []model.ImageModel
	if err := h.DB.Order("sort_order asc, id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	out := make([]adminModelResponse, 0, len(rows))
	for _, m := range rows {
		out = append(out, toAdminModelResponse(m))
	}
	c.JSON(http.StatusOK, gin.H{"models": out})
}

// createModelRequest 新增模型。
//
// Enabled 是指针：省略时默认上架（新接的模型通常就是要用），但要保留"建好先不
// 上架、配置确认后再开"的能力，而非指针无法表达"省略"。
type createModelRequest struct {
	ID            string `json:"id" binding:"required"`
	DisplayName   string `json:"displayName" binding:"required"`
	Provider      string `json:"provider" binding:"required"`
	UpstreamModel string `json:"upstreamModel" binding:"required"`
	// Credits 刻意**不加** binding:"required"：required 对 int 等于"非零"，会把
	// credits=0 归成笼统的"invalid request body"，而这恰恰是最需要解释清楚的一种
	// 错误（0 不是免费，是每次都失败）。留给下面的显式校验给出可读的 message。
	Credits              int   `json:"credits"`
	SupportsImageToImage bool  `json:"supportsImageToImage"`
	Enabled              *bool `json:"enabled"`
	SortOrder            int   `json:"sortOrder"`
}

func (h *AdminModelsHandler) Create(c *gin.Context) {
	var req createModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}
	if req.Credits < minModelCredits {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    errCodeBadRequest,
			"message": "credits 必须 ≥ 1：扣费路径拒绝 cost <= 0，credits=0 会让该模型每次生成都失败",
		})
		return
	}
	// provider 必须已注册 adapter，否则模型建出来在列表里看着正常，用户一点生成就 500。
	if _, err := h.Adapters.Get(req.Provider); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    errCodeBadRequest,
			"message": "provider 没有注册 adapter：该模型建出来后每次生成都会失败",
		})
		return
	}

	// 先查重再插入。**不能用 FirstOrCreate 或 Save**：那会把已存在的线上模型整行
	// 静默覆盖（扣费、上游模型名一起被冲掉），而调用方看到的是 200。
	var existing model.ImageModel
	err := h.DB.Where("id = ?", req.ID).First(&existing).Error
	if err == nil {
		c.JSON(http.StatusConflict, gin.H{"code": 40900, "message": "model id already exists"})
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	m := model.ImageModel{
		ID:                   req.ID,
		DisplayName:          req.DisplayName,
		Provider:             req.Provider,
		UpstreamModel:        req.UpstreamModel,
		Credits:              req.Credits,
		SupportsImageToImage: req.SupportsImageToImage,
		Enabled:              enabled,
		SortOrder:            req.SortOrder,
	}
	// 建行 +（必要时）关掉 enabled，放在**一个事务**里。
	//
	// 为什么要补一条 UPDATE：ImageModel.Enabled 带 `default:true` 标签，GORM 插入时会
	// 把零值字段整列省掉、让数据库填默认值 true——所以 enabled:false 的新模型会被建成
	// **已上架**，运营"先建好、确认上游配置正确之后再开"的意图被静默反转，模型立刻
	// 对所有用户可见。实测 Select("*") 和逐列 Select 都绕不过这个跳过规则，只有插入
	// 之后再显式 UPDATE 才行（UPDATE 不受 default 标签影响）。
	//
	// 放进事务是为了不留"短暂上架"的窗口：两条语句之间若被别的请求读到，那一瞬间
	// 这个未验证的模型是可见、可生成的。
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		if !enabled {
			if err := tx.Model(&m).Update("enabled", false).Error; err != nil {
				return err
			}
			m.Enabled = false
		}
		return nil
	})
	if err != nil {
		// 唯一键冲突也会落到这里（查重与插入之间的并发窗口）。不原样回传 err：
		// 那会泄露数据库内部信息。
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, toAdminModelResponse(m))
}

// patchModelRequest 的字段**全是指针**。
//
// 非指针的话，"没传 enabled"和"传了 enabled:false"无法区分——一个只想把 credits
// 从 1 调成 7 的请求会顺手把模型下架，而且没有任何报错：线上模型从列表里消失，
// 没人会联想到是一次改价造成的。
//
// provider 与 upstreamModel 刻意不可改：改它们等于把这一行变成另一个模型，而历史
// generations 行仍按 id 引用它，事后对账会把两批结果混成一个模型的。要换上游就
// 新建一行、把旧行下架。
type patchModelRequest struct {
	DisplayName          *string `json:"displayName"`
	Credits              *int    `json:"credits"`
	Enabled              *bool   `json:"enabled"`
	SortOrder            *int    `json:"sortOrder"`
	SupportsImageToImage *bool   `json:"supportsImageToImage"`
}

func (h *AdminModelsHandler) Patch(c *gin.Context) {
	id := c.Param("id")

	var req patchModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}

	var m model.ImageModel
	if err := h.DB.Where("id = ?", id).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": 40400, "message": "model not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}

	// 用 map 而不是结构体收集要改的列：Updates 传结构体会跳过零值，正好会静默漏掉
	// enabled:false 和 sortOrder:0——运营点了"下架"却毫无反应。
	updates := map[string]any{}
	if req.DisplayName != nil {
		if *req.DisplayName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "displayName 不能为空"})
			return
		}
		updates["display_name"] = *req.DisplayName
	}
	if req.Credits != nil {
		if *req.Credits < minModelCredits {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    errCodeBadRequest,
				"message": "credits 必须 ≥ 1：扣费路径拒绝 cost <= 0，credits=0 会让该模型每次生成都失败",
			})
			return
		}
		updates["credits"] = *req.Credits
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.SortOrder != nil {
		updates["sort_order"] = *req.SortOrder
	}
	if req.SupportsImageToImage != nil {
		updates["supports_image_to_image"] = *req.SupportsImageToImage
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "没有可修改的字段"})
		return
	}

	if err := h.DB.Model(&m).Updates(updates).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	// 重读而不是拿内存里那份回传：让响应反映库里真正的状态，避免"以为改了但没落库"。
	if err := h.DB.Where("id = ?", id).First(&m).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, toAdminModelResponse(m))
}
