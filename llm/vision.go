package llm

// vision 图片输入包装（M3-8，官方 harness 路线——直连多模态 + 硬门禁 + 驱逐）：
// 历史与用户消息只携带轻引用（用户消息 image part 的 einox-att:// 路径 URL，
// 或工具结果 JSON 顶层 "images" 路径标记——read_image 产出），本包装在每次
// 模型调用前统一处理：
//   ① 门禁：模型未声明图片输入而请求含图 → 硬错误（不自动改投，切换须用户
//     显式换模型——会话模型是创建时快照，含图请求即拒绝）
//   ② 解析：引用 → base64 part（解析失败降级为文本占位不毁会话——文档仓库
//     文件可被 move_document 移动，历史引用失效应继续可聊）
//   ③ 驱逐：累计 base64 超预算时最老的图替换为文本占位（长会话不被请求
//     大小上限打死）
//   ④ 升级：工具结果 images 标记 → 紧随其后的合成 user 消息携图（tool 角色
//     不收图，对齐官方 nested dispatch）
// 无图请求原样透传（快路径：不复制不改写，消息零开销）。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// AttRefPrefix 图片引用 URL 方案（engine 构造引用 part，本包请求时解析）。
const AttRefPrefix = "einox-att://"

// ImageResolver 引用路径 → (字节, MIME)；应用注入（文档仓库读取面）。
type ImageResolver func(path string) ([]byte, string, error)

// SupportsImage 模型是否声明图片输入。
func SupportsImage(m ModelSpec) bool {
	for _, in := range m.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// maxRequestImageBytes 单请求累计图片 base64 预算（对齐官方默认 20MiB；
// var 供测试收窄）。
var maxRequestImageBytes = 20 << 20

// NewVisionModel 图片输入包装（spec = 目标模型能力声明；resolve = 引用解析，
// nil = 未注入——含图请求即错误面）。
func NewVisionModel(inner model.BaseModel[*schema.Message], spec ModelSpec, resolve ImageResolver) model.BaseModel[*schema.Message] {
	return &visionModel{inner: inner, spec: spec, resolve: resolve}
}

type visionModel struct {
	inner   model.BaseModel[*schema.Message]
	spec    ModelSpec
	resolve ImageResolver
}

func (v *visionModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msgs, err := v.transform(input)
	if err != nil {
		return nil, err
	}
	return v.inner.Generate(ctx, msgs, opts...)
}

func (v *visionModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msgs, err := v.transform(input)
	if err != nil {
		return nil, err
	}
	return v.inner.Stream(ctx, msgs, opts...)
}

// markerLabel 合成 user 消息的标注文本（模型可辨识图片来自工具读取）。
const markerLabel = "（read_image 读取的图片随工具结果附上，可直接查看）"

// imgSlot 一张待处理图（用户消息引用 part 或工具标记路径），按消息序排列。
type imgSlot struct {
	mi   int // 所属消息位（输入序列）
	pi   int // 用户消息 part 位 / 标记图在合成消息内的序
	path string
	b64  string
	mime string
	ok   bool // 解析成功
	keep bool // 驱逐后保留
}

// transform 单次模型调用的输入改写（无图透传；详见包注释①~④）。
func (v *visionModel) transform(input []*schema.Message) ([]*schema.Message, error) {
	var slots []imgSlot
	markerMsgs := map[int]bool{} // 含 images 标记的工具消息位
	for i, m := range input {
		switch {
		case m.Role == schema.User:
			for j, p := range m.UserInputMultiContent {
				if p.Type != schema.ChatMessagePartTypeImageURL || p.Image == nil || p.Image.URL == nil {
					continue
				}
				if path, ok := strings.CutPrefix(*p.Image.URL, AttRefPrefix); ok {
					slots = append(slots, imgSlot{mi: i, pi: j, path: path})
				}
			}
		case m.Role == schema.Tool && strings.Contains(m.Content, `"images"`):
			var probe struct {
				Images []string `json:"images"`
			}
			if json.Unmarshal([]byte(m.Content), &probe) != nil {
				continue
			}
			for k, p := range probe.Images {
				if p == "" {
					continue
				}
				markerMsgs[i] = true
				slots = append(slots, imgSlot{mi: i, pi: k, path: p})
			}
		}
	}
	if len(slots) == 0 {
		return input, nil
	}
	if !SupportsImage(v.spec) {
		return nil, fmt.Errorf("模型 %s 不支持图片输入——本请求含图片，请在模型选择器切换到图片输入模型后重发", v.spec.ID)
	}
	if v.resolve == nil {
		return nil, fmt.Errorf("图片引用无法解析（引擎未注入 ImageResolve）")
	}
	type resolved struct {
		b64, mime string
		ok        bool
	}
	cache := map[string]resolved{}
	for i := range slots {
		r, hit := cache[slots[i].path]
		if !hit {
			if b, mime, err := v.resolve(slots[i].path); err == nil && len(b) > 0 {
				r = resolved{base64.StdEncoding.EncodeToString(b), mime, true}
			}
			cache[slots[i].path] = r
		}
		slots[i].b64, slots[i].mime, slots[i].ok = r.b64, r.mime, r.ok
	}
	// 驱逐：从最新向最旧贪心保留（最新一张必留——单图超预算也送出，由端点
	// 裁决）；首次装不下即封口，更老的全部占位。
	cum, open := 0, true
	for i := len(slots) - 1; i >= 0; i-- {
		if !slots[i].ok {
			continue // 解析失败：文本占位，不占预算
		}
		if open && (cum == 0 || cum+len(slots[i].b64) <= maxRequestImageBytes) {
			slots[i].keep = true
			cum += len(slots[i].b64)
		} else {
			open = false
		}
	}
	// 组装：改动的消息克隆改写（历史共享，绝不原地变更）；标记工具消息后
	// 追加合成 user 携图消息。
	slotAt := map[int][]int{} // 消息位 → slots 下标（按扫描序）
	for idx, s := range slots {
		slotAt[s.mi] = append(slotAt[s.mi], idx)
	}
	out := make([]*schema.Message, 0, len(input)+len(markerMsgs))
	for i, m := range input {
		idxs, has := slotAt[i]
		if has && m.Role == schema.User {
			parts := make([]schema.MessageInputPart, len(m.UserInputMultiContent))
			copy(parts, m.UserInputMultiContent)
			for _, idx := range idxs {
				s := slots[idx]
				if s.ok && s.keep {
					parts[s.pi] = schema.MessageInputPart{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
							Base64Data: &s.b64, MIMEType: s.mime,
						}},
					}
				} else {
					parts[s.pi] = schema.MessageInputPart{Type: schema.ChatMessagePartTypeText, Text: placeholderOf(s)}
				}
			}
			clone := *m
			clone.UserInputMultiContent = parts
			clone.Content = "" // openai 适配 Content 与 parts 并存即 MarshalJSON 拒绝——文本只在 text part
			out = append(out, &clone)
			continue
		}
		out = append(out, m)
		if markerMsgs[i] { // 标记工具消息：存活的图随合成 user 消息附上
			syn := []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: markerLabel}}
			for _, idx := range idxs {
				if s := slots[idx]; s.ok && s.keep {
					syn = append(syn, schema.MessageInputPart{
						Type: schema.ChatMessagePartTypeImageURL,
						Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{
							Base64Data: &s.b64, MIMEType: s.mime,
						}},
					})
				}
			}
			if len(syn) > 1 {
				out = append(out, &schema.Message{
					Role: schema.User, UserInputMultiContent: syn, // Content 空：同上并存即拒
				})
			}
		}
	}
	return out, nil
}

// placeholderOf 驱逐/解析失败的占位文本（模型仍保有路径线索）。
func placeholderOf(s imgSlot) string {
	if !s.ok {
		return "（图片 " + s.path + " 读取失败，已跳过）"
	}
	return fmt.Sprintf("（图片 %s 已省略：累计图片超出 %dMB 请求预算）", s.path, maxRequestImageBytes>>20)
}
