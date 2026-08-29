package engine

// 幻觉工具兜底：模型调用了不存在的工具名时，eino ToolsNode 默认行为是硬
// 错误终止整轮且模型不可见（compose tool_node.go「tool %s not found」）。
// 本 handler 把 miss 转为 {"ok":false} 结果信封回喂模型自纠——codex
// RespondToModel / nanobot did-you-mean+自纠后缀 / dsh 路由指引 的三方
// 共识形态。两种 miss 分流（借 dsh 折叠工具思路）：
//   - toolsearch 名单内工具未检索 → 指引先 tool_search（名字可见但被动态
//     装载分流，不是坏工具）；
//   - 名单外 → 真幻觉：报可用名单 + 归一化拼写建议（nanobot：仅提示绝不
//     代执行——唯一命中才提示）。
// 主面 / 拓扑子面 / spawn 子面三处同挂（与 ModelRetryConfig 同策略）。

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jumeng/einox/contract"
)

// newUnknownToolHandler 兜底 handler 构造（known = 静态面全量名 + 动态装载
// 名单；dynamic = 动态装载名集合，nil = 未配 toolsearch）。
func newUnknownToolHandler(known []string, dynamic map[string]bool) func(context.Context, string, string) (string, error) {
	sorted := append([]string(nil), known...)
	sort.Strings(sorted) // 确定性顺序（回放/测试稳定，提示词缓存友好)
	return func(_ context.Context, name, _ string) (string, error) {
		return unknownToolEnvelope(name, sorted, dynamic), nil
	}
}

// unknownToolEnvelope miss 信封构造。
func unknownToolEnvelope(name string, known []string, dynamic map[string]bool) string {
	var b strings.Builder
	b.WriteString("工具 ")
	b.WriteString(name)
	b.WriteString(" 不存在")
	if dynamic != nil && dynamic[name] {
		b.WriteString("——它在动态装载名单内：先调用 tool_search 检索，检索结果中的工具才可调用")
	} else if hint := didYouMean(name, known); hint != "" {
		b.WriteString("（是否想调用 ")
		b.WriteString(hint)
		b.WriteString("？）")
	}
	b.WriteString("。可用工具：")
	b.WriteString(strings.Join(known, ", "))
	b.WriteString("。请改用清单内工具或修正名称后重试，不要重复调用不存在的工具。")
	out, _ := json.Marshal(map[string]any{"ok": false, "error": b.String()})
	return string(out)
}

// didYouMean 归一化拼写建议：小写且只留字母数字（大小写/标点/下划线变体
// 可命中），相等才算命中且唯一命中才提示（多义不猜——nanobot「仅提示绝不
// 执行」纪律；真拼写错误不猜，宁缺毋误）。
func didYouMean(name string, known []string) string {
	want := normalizeToolName(name)
	if want == "" {
		return ""
	}
	hit := ""
	for _, k := range known {
		if normalizeToolName(k) != want {
			continue
		}
		if hit != "" && hit != k {
			return "" // 多个候选：不猜
		}
		hit = k
	}
	return hit
}

// normalizeToolName 归一化（拼写建议专用，不参与执行寻址）：只留小写字母数字。
func normalizeToolName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// contractToolNames 契约工具面名清单（topology/spawn 子面 handler 入参）。
func contractToolNames(ts []contract.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		if info := t.Info(); info != nil && info.Name != "" {
			out = append(out, info.Name)
		}
	}
	return out
}
