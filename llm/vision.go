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
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
)

// AttRefPrefix 图片引用 URL 方案（engine 构造引用 part，本包请求时解析）。
const AttRefPrefix = "einox-att://"

// occFragment 引用尾部的 occurrence 标识片段（#o=N——MintAttRef 铸造；解析
// 后从 resolve 路径剥除）。
const occFragment = "#o="

var occSeq atomic.Uint64

// MintAttRef 铸造带 occurrence 标识的图片引用（同一仓库路径多次进会话各得
// 各的身份——C3 粘性省略按 occurrence 生效，重读产生全新 occurrence 不受旧
// 决策牵连）。用户附件通道由 engine 铸造；应用侧 read_image 类工具可在
// images 标记里直接采纳同款（路径带 #o=N 片段即得粘性语义）。
func MintAttRef(path string) string {
	return AttRefPrefix + path + occFragment + strconv.FormatUint(occSeq.Add(1), 10)
}

// splitOcc 引用/标记 → (净路径, occurrence 标识〔无 = 空〕)。
func splitOcc(ref string) (string, string) {
	if i := strings.Index(ref, occFragment); i >= 0 {
		return ref[:i], ref[i+len(occFragment):]
	}
	return ref, ""
}

// ImageOffloadSink 驱逐决策的会话承载面（engine 经 ctx 注入——beginTurn 与
// emitFn 同点；nil = 无：子面/摘要面/测试装配保持既有零变化行为——预算内
// 现算、不粘滞、不落流）。OffloadedIDs 返回粘性集快照；NoteImageOffload
// 上报本轮新驱逐的 occurrence（引擎侧落事件并入集；实现应吞错——决策承载
// 非关键面，失败即本轮非持久，语义降级不炸请求）。
type ImageOffloadSink interface {
	OffloadedIDs() map[string]bool
	NoteImageOffload(items []contract.ImageOffloadItem)
}

type imageOffloadSinkCtxKey struct{}

// WithImageOffloadSink 把会话承载面塞进 runCtx（vision 包装请求时取用）。
func WithImageOffloadSink(ctx context.Context, sink ImageOffloadSink) context.Context {
	return context.WithValue(ctx, imageOffloadSinkCtxKey{}, sink)
}

func imageOffloadSinkFrom(ctx context.Context) ImageOffloadSink {
	sink, _ := ctx.Value(imageOffloadSinkCtxKey{}).(ImageOffloadSink)
	return sink
}

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
	msgs, err := v.transform(ctx, input)
	if err != nil {
		return nil, err
	}
	return v.inner.Generate(ctx, msgs, opts...)
}

func (v *visionModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msgs, err := v.transform(ctx, input)
	if err != nil {
		return nil, err
	}
	return v.inner.Stream(ctx, msgs, opts...)
}

// markerLabel 合成 user 消息的标注文本（模型可辨识图片来自工具读取）。
const markerLabel = "（read_image 读取的图片随工具结果附上，可直接查看）"

// imgSlot 一张待处理图（用户消息引用 part 或工具标记路径），按消息序排列。
type imgSlot struct {
	mi     int // 所属消息位（输入序列）
	pi     int // 用户消息 part 位 / 标记图在合成消息内的序
	path   string
	occ    string // occurrence 标识（#o= 片段；空 = 未铸造——粘性面不覆盖）
	b64    string
	mime   string
	ok     bool // 解析成功
	keep   bool // 驱逐后保留
	sticky bool // 粘性集命中（先前已驱逐——恒占位，不进预算不计新决策）
}

// transform 单次模型调用的输入改写（无图透传；详见包注释①~④；③的驱逐决策
// 经 ctx 注入的 ImageOffloadSink 粘滞+落流——C3）。
func (v *visionModel) transform(ctx context.Context, input []*schema.Message) ([]*schema.Message, error) {
	var slots []imgSlot
	markerMsgs := map[int]bool{} // 含 images 标记的工具消息位
	for i, m := range input {
		switch {
		case m.Role == schema.User:
			for j, p := range m.UserInputMultiContent {
				if p.Type != schema.ChatMessagePartTypeImageURL || p.Image == nil || p.Image.URL == nil {
					continue
				}
				if ref, ok := strings.CutPrefix(*p.Image.URL, AttRefPrefix); ok {
					path, occ := splitOcc(ref)
					slots = append(slots, imgSlot{mi: i, pi: j, path: path, occ: occ})
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
				path, occ := splitOcc(p)
				markerMsgs[i] = true
				slots = append(slots, imgSlot{mi: i, pi: k, path: path, occ: occ})
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
	// 粘性集命中：先前已驱逐的 occurrence 恒占位（不解析不占预算——占位文本
	// 与首次驱逐逐字节同文，前缀稳定；恢复 = 重读产生新 occurrence）。
	sink := imageOffloadSinkFrom(ctx)
	var sticky map[string]bool
	if sink != nil {
		sticky = sink.OffloadedIDs()
		for i := range slots {
			if slots[i].occ != "" && sticky[slots[i].occ] {
				slots[i].sticky = true
			}
		}
	}
	type resolved struct {
		b64, mime string
		ok        bool
	}
	cache := map[string]resolved{}
	for i := range slots {
		if slots[i].sticky {
			continue // 粘性占位无需解析
		}
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
		if slots[i].sticky || !slots[i].ok {
			continue // 粘性/解析失败：文本占位，不占预算
		}
		if open && (cum == 0 || cum+len(slots[i].b64) <= maxRequestImageBytes) {
			slots[i].keep = true
			cum += len(slots[i].b64)
		} else {
			open = false
		}
	}
	// 新驱逐上报（粘性决策持久化——含铸造身份的 occurrence 才进；上报后即
	// 粘滞，后续请求不再重复上报）。
	if sink != nil {
		var evicted []contract.ImageOffloadItem
		for _, s := range slots {
			if s.occ != "" && s.ok && !s.keep && !s.sticky {
				evicted = append(evicted, contract.ImageOffloadItem{ID: s.occ, Path: s.path})
			}
		}
		if len(evicted) > 0 {
			sink.NoteImageOffload(evicted)
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

// placeholderOf 驱逐/解析失败的占位文本（模型仍保有路径线索）。粘性命中走
// 预算文案——与首次驱逐逐字节同文（前缀稳定）；恢复通道 = 重读该路径产生
// 全新 occurrence。
func placeholderOf(s imgSlot) string {
	if s.sticky {
		return fmt.Sprintf("（图片 %s 已省略：累计图片超出 %dMB 请求预算）", s.path, maxRequestImageBytes>>20)
	}
	if !s.ok {
		return "（图片 " + s.path + " 读取失败，已跳过）"
	}
	return fmt.Sprintf("（图片 %s 已省略：累计图片超出 %dMB 请求预算）", s.path, maxRequestImageBytes>>20)
}
