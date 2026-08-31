package askuser

// checkpoint 序列化兼容回归（A3）：askState 经 checkpoint 的 `any` 接口位 gob
// 往返——字段重命名/改型会静默破坏存量 checkpoint。登记处：askState 经
// schema.Register 注册（askuser.go:32），本测试锚住字段面不漂移。

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/jumeng/einox/contract"
)

func TestAskStateGobRoundTrip(t *testing.T) {
	st := askState{Info: contract.AskCard{
		Question: "选哪条路线？", AllowMulti: true,
		Options: []contract.AskOption{{Label: "A", Value: "a"}, {Label: "B", Value: "b"}},
	}}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(struct{ State any }{st}); err != nil {
		t.Fatalf("编码失败（接口位）：%v", err)
	}
	var out struct{ State any }
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	got, ok := out.State.(askState)
	if !ok {
		t.Fatalf("往返类型不符：%T", out.State)
	}
	if got.Info.Question != st.Info.Question || len(got.Info.Options) != 2 || got.Info.Options[1].Value != "b" {
		t.Fatalf("字段漂移（重命名会静默破坏存量 checkpoint）：%+v", got)
	}
}
