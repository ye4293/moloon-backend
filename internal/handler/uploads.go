package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"image-backend/internal/middleware"
	"image-backend/internal/storage"
)

const (
	// errCodeStorageUnavailable 对象存储没配好。**单独一个码，不复用 50000。**
	//
	// 这两种失败的处置完全不同：50000 让人去查代码和日志，而这一条的意思是
	// "去后台把 R2 那五项填上"。混在一起会让一次配置缺失以"服务器内部错误"的形式
	// 出现，把排查方向带到最没用的地方。
	errCodeStorageUnavailable = 50302
	// errCodeUploadTooLarge 图片超过上限。
	errCodeUploadTooLarge = 41300
)

// maxReferenceDataURLChars data URL 字符串的字符数上限。
//
// **在解码之前按字符数拦一次。** 先解码再判断大小等于"拦住"发生在内存已经被占掉
// 之后——而这是个未授权用户拿不到、但任何已登录用户都能反复打的入口，
// 一个 50MB 的 base64 串乘上并发就是内存耗尽。
//
// base64 每 4 个字符编码 3 字节，所以 1.4MB 字符 ≈ 1.05MB 原始字节。取这个数是因为
// 浏览器端已经把图压到 ≤900KB（见前端的 compressImage），1.05MB 留了约 1.16 倍余量；
// 同时它明显低于宿主 nginx 的 client_max_body_size 2m，所以正常请求不会先被 nginx
// 以一个没有信息的 413 拦掉。
//
// 三者的关系是刻意的：**浏览器端上限 < 这里 < nginx 上限**。任何一层收紧到低于
// 前一层，症状都会变成"前端以为自己压够了，却拿到一个说不清原因的失败"。
const maxReferenceDataURLChars = 1_400_000

// UploadsHandler 用户上传参考图（图生图 / 编辑的输入）。
//
// **返回不透明的对象键，不返回 URL。** 这是这个接口最重要的一个设计选择，理由是
// 生成接口那一侧的安全性：如果这里返回 URL、生成请求再把 URL 传回来，我们就必须
// 校验"这个 URL 属于我们自己的 R2"——而 internal/config 的 validateOrigin 注释记录了
// 上一次做同类校验的教训：后缀匹配无法可靠地锚定在域名标签边界上，代码审查在那一版
// 实现里跑出了四类绕过。校验漏了的后果是用户能让上游去抓取任意 URL（SSRF）。
//
// 返回键之后，URL 完全由后端从自己的配置拼出来（storage.PublicURL），用户可控的
// 部分只剩一个键，而键的合法形状可以精确匹配（见 generations.go 的 parseReferenceKey）。
// 前端要显示缩略图用本地的 URL.createObjectURL 就够了，压根不需要这个 URL。
type UploadsHandler struct {
	// Refs 按请求取一次当前生效的存储与公开域名，于是后台改完 R2 配置立刻生效
	// ——与 GenerationsHandler.Adapters 同一个约定。
	Refs func() ReferenceStore
}

// ReferenceStore 上传参考图需要的两样东西：往哪写、以及怎么把键变回 URL。
//
// **绑成一个结构体、由一个 getter 一次性返回**，而不是两个独立的 getter：它们单独
// 都没有用（没有公开域名的 Storage 产不出可访问的 URL），而分两次从
// settings.Runtime 取会在热重载的瞬间拿到不匹配的一对——用新的存储写对象、
// 用旧的域名拼 URL，结果是一个 404 的永久链接，且没有任何地方报错。
type ReferenceStore struct {
	Store      storage.Storage
	PublicBase string
}

type uploadReferenceRequest struct {
	// Image 形如 data:image/jpeg;base64,<...>
	//
	// 收 data URL 而不是 multipart，是因为 multipart/form-data 是 CORS **简单请求**
	// 类型、**不触发预检**（与 middleware/jsononly.go 里对 text/plain 的论证同源）。
	// 走 application/json 之后这条路由能挂上 JSONOnly，跨域页面必须先过预检，
	// 而预检会被 CORS 白名单拦下。
	Image string `json:"image" binding:"required"`
}

// Create 收一张图，校验后存进对象存储，返回对象键。
func (h *UploadsHandler) Create(c *gin.Context) {
	userID := c.GetUint(middleware.CtxUserIDKey)

	var req uploadReferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    errCodeBadRequest,
			"message": "invalid request body",
		})
		return
	}

	// 长度检查在解码之前（理由见 maxReferenceDataURLChars）。
	if len(req.Image) > maxReferenceDataURLChars {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"code": errCodeUploadTooLarge,
			"message": fmt.Sprintf(
				"图片太大（%d 字符，上限 %d）。请压缩后再上传",
				len(req.Image), maxReferenceDataURLChars),
		})
		return
	}

	body, err := decodeImageDataURL(req.Image)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": err.Error()})
		return
	}

	// **嗅探字节，不看 data URL 里声明的类型。** 声明值由发送方随意填写，
	// 一个 data:image/png 开头、内容是 HTML 的串是一次普通请求，不需要任何技巧。
	// 落地用的扩展名也取嗅探结果，于是"声明 png、实际 jpeg"会正确地存成 .jpg。
	ct, ext, err := storage.SniffImageType(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": err.Error()})
		return
	}

	refs := h.Refs()
	// 键里带 userID 段：生成接口据此确认"这个键是调用者自己上传的"，
	// 否则 A 能引用 B 上传的图（见 generations.go 的 parseReferenceKey）。
	// uuid 而不是自增或哈希：键会以公开 URL 的形式暴露，可枚举的键等于让任何人
	// 遍历别人上传的图片。
	key := referenceKey(userID, ext)

	// **先校验再写。** 顺序反了的话，一个被拒的 HTML 文件已经落在我们自己的域名下了
	// ——接口仍然返回 400，看起来一切正常，而 origin 上多了一个别人可控的文件。
	url, err := refs.Store.Put(c.Request.Context(), key, ct, body)
	if err != nil {
		if errors.Is(err, storage.ErrNotConfigured) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"code": errCodeStorageUnavailable,
				"message": "reference images require object storage; " +
					"配置后台设置页里的 R2 五项后即可使用",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	// Put 成功但公开域名为空的话，URL 拼不出来。这一支正常不可达
	// （StorageEnabled 要求 r2PublicBaseUrl 非空，否则拿到的是 NoopStorage），
	// 留着是因为"能写进去但拼不出地址"会产出一个永远打不开的参考图，
	// 而那个失败要到上游拉取时才以一个笼统的上游错误出现。
	if url == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"code":    errCodeStorageUnavailable,
			"message": "reference images require a public storage domain",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"key": key})
}

// referenceKey 参考图的对象键：ref/<userID>/<uuid>.<ext>
//
// 前缀独立于生成结果的 `g/`：两者的生命周期不同（参考图是用户的输入，生成结果是
// 付过费的产物），分开才能在 R2 上给它们配不同的 lifecycle 规则。
func referenceKey(userID uint, ext string) string {
	return "ref/" + strconv.FormatUint(uint64(userID), 10) + "/" + uuid.NewString() + "." + ext
}

// decodeImageDataURL 解析 `data:image/<sub>;base64,<payload>` 并解码出字节。
//
// 只接受 base64 编码的 image/* data URL：
//
//   - 不接受裸 base64。上游（BFL）要求 input_image 是 URI，裸 base64 会被它以
//     `Does not match format 'uri'` 拒掉；在我们这一层就统一成 data URL，
//     省掉一次"到了上游才失败"的往返。
//   - 不接受非 base64 的 data URL（如 percent-encoding）。多一种编码就多一条
//     需要独立验证的解析路径，而浏览器端 canvas.toDataURL 只产出 base64。
//   - **声明的 MIME 不被信任**，只用来快速拒掉明显不是图片的请求；真正的判定
//     在 storage.SniffImageType。
func decodeImageDataURL(s string) ([]byte, error) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return nil, errors.New("image 必须是 data URL（形如 data:image/jpeg;base64,...）")
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, errors.New("image 不是合法的 data URL：缺少逗号分隔的载荷")
	}
	meta, payload := s[len(prefix):comma], s[comma+1:]
	if !strings.HasSuffix(meta, ";base64") {
		return nil, errors.New("image 的 data URL 必须是 base64 编码")
	}
	mime := strings.TrimSuffix(meta, ";base64")
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("image 声明的类型是 %q，只接受 image/*", mime)
	}
	body, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, errors.New("image 的 base64 载荷解码失败")
	}
	return body, nil
}
