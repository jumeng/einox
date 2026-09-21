package engine

// C2 read→spill→read 防环回归（dsh spill-policy「模型面跳过 read 结果」同
// 裁决）：外置指针若被再外置即自引用环——模型按指针取回的是又一张指针卡。
// read_file 自身结果禁截断外置；读结果的有界面归 fsutil 窗口参数
//（offset/limit/line_width）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools"
)

// TestReductionReadResultsNotOffloaded read_file 结果原样内联：big_tool 结果
// 外置换指针后，模型按指针 read_file 取回——取回结果不得被再外置（防环）。
func TestReductionReadResultsNotOffloaded(t *testing.T) {
	big, _ := tools.InferTool("big_tool", "大结果桩", func(context.Context, struct{}) (map[string]any, error) {
		return map[string]any{"data": strings.Repeat("RAWMARK", 2000)}, nil // 14k 单行——超截断线
	})
	fm := &scriptedModel{onStream: func(n int, send func(*schema.Message)) {
		switch n {
		case 1:
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{tcOf("c1", "big_tool", `{}`)}})
		case 2: // 模型按截断通知的指针取回全文——显式放宽行宽（单行 JSON 缺省
			// 2000 宽会被 fsutil 截短到阈值以下，宽行读才会把 14k 原文带回
			// 出站面；多行 spill 文件〔transcript/clear 块〕缺省窗口同理可超）
			send(&schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
				tcOf("c2", "read_file", `{"path":"spill/trunc/c1","line_width":50000}`)}})
		default:
			send(&schema.Message{Role: schema.Assistant, Content: "完成"})
		}
	}}
	m, _ := newReductionManager(t, 0, []contract.Tool{big}, factoryOf(fm))
	s := m.Registry().Create("张三", "防环", "plan", contract.UserPrefs{Model: "p/m"})
	s.SetState(session.StateRunning)
	var errMsg string
	m.Run(context.Background(), s, "跑大工具", nil, func(ev session.Event) {
		if ev.Event == contract.EvError {
			errMsg = fmt.Sprintf("%v", ev.Data)
		}
	})
	waitTitleFlight(t, s)
	if len(fm.inputsOf()) != 3 {
		t.Fatalf("模型应调用 3 次，实得 %d；错误 %s", len(fm.inputsOf()), errMsg)
	}
	// 参照面：big_tool 结果照旧外置（指针卡在场——机制对非 read 工具不变）
	if c1 := toolMsgOfID(fm.inputsOf()[2], "c1"); !strings.Contains(c1, "spill/trunc/c1") {
		t.Fatalf("big_tool 结果应照旧外置换指针：%s", head(c1, 120))
	}
	// 防环面：read_file 结果 = 原文内联——不得出现自引用指针（再外置即环：
	// 模型按指针取回的是又一张指针卡），且全量标记在场（截断替换的头尾预览
	// 仅 ~8k 字符 ≈ 1170 标记，原文 2000——阈值 1600 区分）。
	c2 := toolMsgOfID(fm.inputsOf()[2], "c2")
	if strings.Contains(c2, "spill/trunc/c2") {
		t.Fatalf("read_file 结果被再外置（自引用指针卡——防环失效）：%s", head(c2, 120))
	}
	if n := strings.Count(c2, "RAWMARK"); n < 1600 {
		t.Fatalf("read_file 结果应原样内联（防环）：RAWMARK×%d（<1600 即被截断替换）；头=%s", n, head(c2, 120))
	}
}

// toolMsgOf 取输入序列中指定 ToolCallID 的 tool 消息内容（空 = 未找到）。
func toolMsgOfID(msgs []*schema.Message, callID string) string {
	for _, m := range msgs {
		if m.Role == schema.Tool && m.ToolCallID == callID {
			return m.Content
		}
	}
	return ""
}
