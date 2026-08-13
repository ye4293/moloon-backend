// Package storage 把生成好的图片转存到我们自己控制的对象存储。
//
// 存在理由：上游返回的图片 URL 指向它自己的 CDN，约一小时后失效。不转存的话
// 历史记录里全是死链，而用户为那些图付过费。
package storage

import (
	"context"
	"errors"
	"strings"
)

// ErrNotConfigured NoopStorage 的固定返回，调用方据此走降级路径。
var ErrNotConfigured = errors.New("storage is not configured")

type Storage interface {
	// Put 上传并返回可公开访问的永久 URL。
	//
	// body 收 []byte 而不是 io.Reader：aws-sdk-go-v2 的 PutObject 需要可重放的
	// body 才能签名和重试，给它一个不可 seek 的 reader 会让 SDK 自己先缓冲一遍
	// ——同一份数据在内存里两份。反正上游就是一张图、调用方本来就要限大小，
	// 直接收字节更诚实。
	Put(ctx context.Context, key, contentType string, body []byte) (string, error)
}

// NormalizeKey 去掉对象键开头的斜杠。
//
// publicBase 已经去掉了末尾斜杠（见 NewR2Storage），两边各留一个就会拼出 `//`，
// 有些 CDN 对此 404 —— 而这个 URL 是要永久存进库里的。归一化后的键同时用于
// PutObject 与拼 URL，避免"对象键与 URL 各说一套"。
func NormalizeKey(key string) string { return strings.TrimPrefix(key, "/") }

// PublicURL 把对象键拼成公开可访问的 URL。
//
// **这是拼接规则的唯一实现处。** R2Storage.Put 与"把已上传的参考图键还原成 URL"
// （internal/handler/uploads.go、generations.go）都必须走它：两份拼接实现走偏的表现
// 是一部分 URL 多一个或少一个斜杠，而那不会报错，只会在浏览器里 404，
// 并且是在数据已经入库之后。
//
// publicBase 为空（storage 未配置）时返回空串，让调用方走"拼不出可访问 URL"的
// 降级路径，而不是产出一个 `/r/1/x.jpg` 这样会被浏览器当成相对路径的东西。
func PublicURL(publicBase, key string) string {
	if publicBase == "" {
		return ""
	}
	return strings.TrimSuffix(publicBase, "/") + "/" + NormalizeKey(key)
}
