package storage

import (
	"bytes"
	"strings"
	"testing"
)

// 各格式的最小可嗅探字节。http.DetectContentType 只看前 512 字节的魔数，
// 所以不需要构造完整的合法图片文件。
var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n")
	jpegBytes = []byte("\xff\xd8\xff\xe0")
	// WebP 的嗅探需要 "RIFF" + 4 字节长度 + "WEBPVP"。
	webpBytes = []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
)

func TestSniffImageTypeAcceptsWhitelistedFormats(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		wantCT  string
		wantExt string
	}{
		{"png", pngBytes, "image/png", "png"},
		{"jpeg", jpegBytes, "image/jpeg", "jpg"},
		{"webp", webpBytes, "image/webp", "webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, ext, err := SniffImageType(tc.body)
			if err != nil {
				t.Fatalf("应当接受 %s: %v", tc.name, err)
			}
			if ct != tc.wantCT || ext != tc.wantExt {
				t.Errorf("得到 (%q, %q)，想要 (%q, %q)", ct, ext, tc.wantCT, tc.wantExt)
			}
		})
	}
}

func TestSniffImageTypeRejectsNonImages(t *testing.T) {
	// 每一条都是"能挂到我们自己域名下"的实际风险，不是理论输入。
	for _, tc := range []struct {
		name string
		body []byte
		why  string
	}{
		{
			"html",
			[]byte("<!DOCTYPE html><script>alert(document.cookie)</script>"),
			"HTML 挂在我们自己的 origin 上就是 XSS，能读到同源的一切",
		},
		{
			"伪装成 png 文件名的 html",
			// 文件名与声明的 Content-Type 都不构成证据，这就是为什么要嗅探字节。
			[]byte("<html><body>looks like x.png but isn't</body></html>"),
			"文件名和请求头都由发送方随意填写，唯一可信的是字节",
		},
		{
			"svg",
			[]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
			"SVG 是**可执行脚本的图片格式**——浏览器直接打开时会跑里面的 <script>，" +
				"所以它必须留在白名单外面，哪怕它确实是一种图片",
		},
		{
			"gif",
			[]byte("GIF89a"),
			"上游不接受 gif，让它进来只会在生成时失败，而那时已经存进对象存储了",
		},
		{
			"pdf",
			[]byte("%PDF-1.7"),
			"不是图片",
		},
		{
			"纯文本",
			[]byte("just some text, definitely not an image at all"),
			"不是图片",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, ext, err := SniffImageType(tc.body)
			if err == nil {
				t.Fatalf("应当拒绝（%s），却得到 (%q, %q)", tc.why, ct, ext)
			}
			// 错误里必须带上嗅探结果：调用方以为自己传的是图片，
			// "嗅探到 text/html" 是唯一能说明发生了什么的线索。
			if !strings.Contains(err.Error(), "嗅探到") {
				t.Errorf("错误信息应当带上嗅探到的类型，得到：%v", err)
			}
		})
	}
}

func TestSniffImageTypeRejectsEmpty(t *testing.T) {
	// 空 body 单独拦。不拦的话 DetectContentType 会返回 "text/plain; charset=utf-8"，
	// 报出来的是"嗅探到 text/plain"——那条消息会让人去查自己传了什么文本，
	// 而真实情况是什么都没传到。
	if _, _, err := SniffImageType(nil); err == nil {
		t.Error("空内容应当被拒绝")
	}
	if _, _, err := SniffImageType([]byte{}); err == nil {
		t.Error("零长度内容应当被拒绝")
	}
}

func TestSniffImageTypeRejectsOversized(t *testing.T) {
	// 超限报错而不是截断：截断出来的是一个坏图片，它会一路存进存储、拿到正常的
	// URL，然后在每个浏览器里显示成半张图——比明确失败坏得多。
	big := append(bytes.Clone(pngBytes), make([]byte, MaxImageBytes)...)
	if len(big) <= MaxImageBytes {
		t.Fatalf("前提不成立：构造的 body 是 %d 字节，没有超过上限 %d", len(big), MaxImageBytes)
	}
	if _, _, err := SniffImageType(big); err == nil {
		t.Errorf("%d 字节超过上限 %d，应当被拒绝", len(big), MaxImageBytes)
	}

	// 边界的另一侧：正好等于上限要放行，否则上限的语义会悄悄变成"上限减一"。
	atLimit := append(bytes.Clone(pngBytes), make([]byte, MaxImageBytes-len(pngBytes))...)
	if len(atLimit) != MaxImageBytes {
		t.Fatalf("前提不成立：构造了 %d 字节，想要正好 %d", len(atLimit), MaxImageBytes)
	}
	if _, _, err := SniffImageType(atLimit); err != nil {
		t.Errorf("正好等于上限应当放行: %v", err)
	}
}

func TestSniffImageTypeStripsContentTypeParameters(t *testing.T) {
	// DetectContentType 对文本类会返回 "text/plain; charset=utf-8" 这种带参数的形式。
	// 白名单是按裸类型建的，所以必须先剥参数再查表。
	//
	// 这条不测"接受"，而是钉住剥参数这一步真的发生了——它对白名单里的三种格式
	// 目前不产生差别（DetectContentType 对它们不带参数），但一旦哪天带了，
	// 不剥参数就会让合法图片被拒，而症状是"我传的明明是 png"。
	ct, _, err := SniffImageType(pngBytes)
	if err != nil {
		t.Fatalf("png 应当被接受: %v", err)
	}
	if strings.ContainsAny(ct, ";") {
		t.Errorf("返回的 content-type 不该带参数，得到 %q", ct)
	}
}
