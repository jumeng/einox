package plan

// checkpoint 序列化兼容回归（A3）：planState 经 checkpoint 的 `any` 接口位 gob
// 往返——字段重命名/改型会静默破坏存量 checkpoint。登记处：planState 经
// schema.Register 注册（plan.go:33），本测试锚住字段面不漂移。

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/jumeng/einox/contract"
)

func TestPlanStateGobRoundTrip(t *testing.T) {
	st := planState{Info: contract.PlanCard{
		Task: "重构检索面", Seq: 3, Path: "plans/s1/3.md",
		Steps: []contract.PlanStep{{Title: "先读现状", Detail: "recall 三模式"}},
	}}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(struct{ State any }{st}); err != nil {
		t.Fatalf("编码失败（接口位）：%v", err)
	}
	var out struct{ State any }
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	got, ok := out.State.(planState)
	if !ok {
		t.Fatalf("往返类型不符：%T", out.State)
	}
	if got.Info.Task != st.Info.Task || got.Info.Seq != 3 || len(got.Info.Steps) != 1 || got.Info.Steps[0].Title != "先读现状" {
		t.Fatalf("字段漂移（重命名会静默破坏存量 checkpoint）：%+v", got)
	}
}
