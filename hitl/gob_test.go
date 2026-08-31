package hitl

// checkpoint 序列化兼容回归（A3）：approvalState 经 checkpoint 的 `any` 接口位
// gob 往返——gob 按字段名编码，字段重命名/改型会**静默**破坏存量 checkpoint
// （挂起-恢复恰是最难测的路径）。登记处：approvalState/ApprovalCard 经
// schema.Register 注册（hitl.go:104-105），本测试锚住字段面不漂移。

import (
	"bytes"
	"encoding/gob"
	"testing"
)

func TestApprovalStateGobRoundTrip(t *testing.T) {
	st := approvalState{Args: `{"path":"a.txt"}`, ItemID: "i3f9a1"}
	var buf bytes.Buffer
	// 与 checkpoint 载荷同位：State 是 any 接口字段，接口位编码即注册生效路径
	if err := gob.NewEncoder(&buf).Encode(struct{ State any }{st}); err != nil {
		t.Fatalf("编码失败（接口位）：%v", err)
	}
	var out struct{ State any }
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	got, ok := out.State.(approvalState)
	if !ok {
		t.Fatalf("往返类型不符：%T", out.State)
	}
	if got.Args != st.Args || got.ItemID != st.ItemID {
		t.Fatalf("字段漂移（重命名会静默破坏存量 checkpoint）：%+v", got)
	}
}
