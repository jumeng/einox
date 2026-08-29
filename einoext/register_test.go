package einoext

// RegisterSuspendState 注册出口回归（设计真源 findings/2026-08-29-assembly-seams-design.md
// §8.1）：Suspend.Info/State 过 checkpoint 走 gob——接口字段持有的具体类型
// 必须注册；未注册类型编码即失败（fail 口），经注册出口登记后往返完好。
// 业务据此不 import eino 即可满足序列化义务。

import (
	"bytes"
	"encoding/gob"
	"testing"
)

type regDemoState struct{ N int }

type unregDemoState struct{ N int }

func TestRegisterSuspendStateGobRoundTrip(t *testing.T) {
	RegisterSuspendState[regDemoState]()

	// 未注册类型经接口字段编码即报错——契约邀请业务 Suspend 的隐含序列化义务
	var buf bytes.Buffer
	holder := struct{ V any }{V: unregDemoState{N: 1}}
	if err := gob.NewEncoder(&buf).Encode(&holder); err == nil {
		t.Fatal("未注册类型应编码失败（gob 接口字段义务）")
	}

	// 已注册类型往返完好
	buf.Reset()
	holder.V = regDemoState{N: 7}
	if err := gob.NewEncoder(&buf).Encode(&holder); err != nil {
		t.Fatalf("已注册类型编码失败：%v", err)
	}
	var out struct{ V any }
	if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
		t.Fatalf("已注册类型解码失败：%v", err)
	}
	if s, ok := out.V.(regDemoState); !ok || s.N != 7 {
		t.Fatalf("往返失真：%+v", out.V)
	}
}
