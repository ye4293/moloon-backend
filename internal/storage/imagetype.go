package storage

import (
	"fmt"
	"net/http"
	"strings"
)

// MaxImageBytes 单个图片对象的字节上限。
//
// 无上限地把字节读进内存是内存耗尽向量：并发几十个请求，加上一个巨大或永不结束的
// 响应体，就能把服务打死。
const MaxImageBytes = 20 << 20 // 20 MiB

// allowedImageTypes 白名单，值是落地用的扩展名。
//
// **白名单而非黑名单**：这些字节要挂到**我们自己的域名**下，能想到要拦什么的人总会
// 漏掉一种，而漏掉的那种如果是 HTML，就是我们自己 origin 上的 XSS。
//
// 这条论证对两个调用方都成立，而对第二个更硬：
//
//   - 转存上游出的图（internal/generation/storing.go）—— 字节由上游控制
//   - 用户上传的参考图（internal/handler/uploads.go）—— **字节完全由用户控制**，
//     也就是说"上传一个 .png 结尾、内容是 HTML 的文件"是一次普通的、无需任何技巧的
//     请求，而不是需要上游配合才能构造的场景
var allowedImageTypes = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
	"image/webp": "webp",
}

// SniffImageType 校验这段字节是否是允许落地的图片，返回规范化的 content-type
// 与落地用的扩展名。
//
// **嗅探内容，不信任调用方给的 Content-Type 或文件名。** 无论请求头里写的是
// image/png、还是文件名以 .png 结尾，都不构成任何证据——两者都由发送方随意填写。
// 唯一可信的是字节本身。
//
// 超过 MaxImageBytes 时报错而不是截断：截断出来的是一个坏图片文件，它会一路存进
// 存储、拿到一个正常的 URL，然后在每个浏览器里显示成半张图——那比明确失败坏得多。
func SniffImageType(body []byte) (contentType, ext string, err error) {
	if len(body) == 0 {
		return "", "", fmt.Errorf("图片内容为空")
	}
	if len(body) > MaxImageBytes {
		return "", "", fmt.Errorf("图片 %d 字节，超过 %d 字节上限", len(body), MaxImageBytes)
	}

	ct := http.DetectContentType(body)
	// DetectContentType 可能返回带参数的形式（如 "text/plain; charset=utf-8"），
	// 白名单是按裸类型建的，先切掉参数。
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	ext, ok := allowedImageTypes[ct]
	if !ok {
		// 把嗅探结果带进错误里。**这是排查这类失败的唯一线索**：调用方以为自己传的是
		// 图片，而"嗅探到 text/html"一眼就能说明发生了什么。
		return "", "", fmt.Errorf("拒绝非图片内容：嗅探到 %q", ct)
	}
	return ct, ext, nil
}
