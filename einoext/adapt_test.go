package einoext

// adaptTool panic 隔离回归：包装链任一层 panic 在适配层单点收敛为
// {"ok":false} 信封（进程不崩、模型可自纠）；Suspend 直通语义不受影响。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"

	"github.com/jumeng/einox/contract"
)

type panicTool struct{ name string }

func (p *panicTool) Info() *contract.ToolInfo {
	return &contract.ToolInfo{Name: p.name}
}

func (p *panicTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	panic("boom 崩了")
}

func TestAdaptPanicBecomesEnvelope(t *testing.T) {
	ad := Adapt([]contract.Tool{&panicTool{name: "crashy"}})[0].(tool.InvokableTool)
	out, err := ad.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("panic 应转结果信封不上抛：%v", err)
	}
	var env struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(out), &env); jerr != nil || env.OK {
		t.Fatalf("信封应为 ok=false：%s（%v）", out, jerr)
	}
	for _, want := range []string{"crashy", "panic"} {
		if !strings.Contains(env.Error, want) {
			t.Fatalf("信封应含工具名与 panic 语义：%s", env.Error)
		}
	}
}

type suspendTool struct{}

func (suspendTool) Info() *contract.ToolInfo { return &contract.ToolInfo{Name: "ask"} }

func (suspendTool) Invoke(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, &contract.Suspend{Info: contract.ApprovalCard{Tool: "ask"}, State: map[string]string{"x": "1"}}
}

func TestAdaptSuspendStillPasses(t *testing.T) {
	ad := Adapt([]contract.Tool{suspendTool{}})[0].(tool.InvokableTool)
	_, err := ad.InvokableRun(context.Background(), `{}`)
	if err == nil {
		t.Fatal("Suspend 应仍以 error 形态直通（不被 recover 吞）")
	}
	// 适配层语义：Suspend 哨兵在此翻译为引擎中断信号（StatefulInterrupt →
	// core.InterruptSignal，adk.InterruptSignal 同型别名）
	var sig *adk.InterruptSignal
	if !errors.As(err, &sig) {
		t.Fatalf("error 应为引擎中断信号（Suspend 翻译产物）：%T", err)
	}
}
