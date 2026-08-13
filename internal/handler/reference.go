package handler

import (
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	"image-backend/internal/storage"
)

// maxReferenceImages 一次生成最多带几张参考图。
//
// 与上游的上限一致（BFL 的 Flux2Inputs 只开到 input_image_8）。在这里也拦一道，
// 是为了让超限以**一条说得清的 400** 出现，而不是等扣完费、请求打到上游之后
// 收一个笼统的 422 —— 那时行已经建了、次数已经扣了，只能靠退款路径找补。
const maxReferenceImages = 8

// parseReferenceKeys 校验并归一化前端传回来的参考图对象键。
//
// **这是 key 方案下的安全边界。** 上传接口刻意返回不透明键而不是 URL，正是为了让
// 这里的校验能做成精确匹配：URL 的归属校验要跟域名后缀、端口、userinfo、
// 百分号编码打交道（internal/config 的 validateOrigin 注释记录了那一版跑出四类
// 绕过的经过），而键的合法形状只有一种，可以逐段核对。
//
// 校验三件事，每一件对应一种真实的攻击或事故：
//
//  1. **形状必须是 `ref/<userID>/<name>`，恰好三段。** 这一条承担了绝大部分安全
//     工作：`ref/1/../../g/x.png`（6 段）、`ref/1/%2e%2e/x.png`（4 段）、
//     `/ref/1/x.png`（首段为空，4 段）全都在这里被拦下，顺带也挡住
//     `g/<别人的生成id>.jpg` 这类跨命名空间引用别人**成品图**的尝试。
//  2. **userID 段必须等于调用者。** 否则 A 能把 B 上传的私人照片拿去做生成——
//     而桶是公开可读的，键一旦泄露（比如出现在别人的历史里）就能被直接引用。
//  3. **必须是规范路径。** 这一条只在段数检查够不到的缝隙里起作用：`ref/1/..`
//     与 `ref/1/.` **恰好也是三段**，却分别指向上一级和本级目录。它们不像前面那些
//     那样危险（对象存储把键当字面量，不会真的解析 `..`），但它们不是任何一次上传
//     产出过的键，放进去只会得到一个 404 的参考图——而那个失败要到上游拉取时才以
//     一条笼统的上游错误出现。
func parseReferenceKeys(keys []string, userID uint) error {
	if len(keys) > maxReferenceImages {
		return fmt.Errorf("最多 %d 张参考图，收到 %d 张", maxReferenceImages, len(keys))
	}
	want := "ref/" + strconv.FormatUint(uint64(userID), 10)
	seen := make(map[string]bool, len(keys))
	for i, k := range keys {
		if k == "" {
			return fmt.Errorf("第 %d 张参考图的键为空", i+1)
		}
		// 规范性检查。path.Clean 会折叠 `.` `..` 与重复斜杠，结果不同即非规范。
		// 见函数注释第 3 条：它只覆盖段数检查够不到的那条缝。
		if path.Clean(k) != k || strings.HasPrefix(k, "/") {
			return fmt.Errorf("第 %d 张参考图的键不是规范路径", i+1)
		}
		parts := strings.Split(k, "/")
		if len(parts) != 3 || parts[0] != "ref" {
			return fmt.Errorf("第 %d 张参考图的键格式不对", i+1)
		}
		if parts[0]+"/"+parts[1] != want {
			// 不把 want 写进错误信息：那等于告诉调用者"正确的前缀长什么样"，
			// 而这条错误的受众是我们自己的前端（它本来就传得对）。
			return fmt.Errorf("第 %d 张参考图不属于当前用户", i+1)
		}
		// 重复的键会让同一张图占掉两个 input_image 槽位——不报错，但用户会发现
		// "我明明选了 8 张不同的图"却只有几张生效。
		if seen[k] {
			return fmt.Errorf("第 %d 张参考图重复了", i+1)
		}
		seen[k] = true
	}
	return nil
}

// referenceURLs 把对象键拼成上游能抓取的公开 URL。**顺序原样保留。**
//
// 顺序对应上游的 input_image、input_image_2…，而"第一张是主体、后面是风格参考"
// 这种语义由用户在界面上的排列决定。任何一层排序或去重都会悄悄改变生成结果。
func referenceURLs(keys []string, publicBase string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, storage.PublicURL(publicBase, k))
	}
	return out
}

// encodeReferenceKeys 序列化成 generations.reference_keys 里存的 JSON 数组。
// 空切片存空串而不是 "[]"，让"没有参考图"在库里一眼可辨。
func encodeReferenceKeys(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	b, err := json.Marshal(keys)
	if err != nil {
		// keys 是 []string，json.Marshal 不会失败。真失败了也不该让一次生成挂掉
		// ——参考图的记录是审计信息，不是生成本身需要的东西。
		return ""
	}
	return string(b)
}

// decodeReferenceKeys 解析 generations.reference_keys。
//
// **解析失败返回 nil 而不是报错。** 这一列是审计信息，一条坏数据（手工改库、
// 或将来换了格式）不该让整个历史列表 500。
func decodeReferenceKeys(raw string) []string {
	if raw == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}
