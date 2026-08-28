package todo

import (
	"context"
	"encoding/json"
	"testing"
)

// 记数桩 Store。
type memStore struct {
	items []Item
}

func (m *memStore) Set(items []Item) { m.items = items }

func invoke(t *testing.T, args string) (map[string]any, *memStore) {
	t.Helper()
	st := &memStore{}
	ts, err := NewTools(Config{Store: st})
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	out, err := ts[0].Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		t.Fatalf("非 JSON 输出：%s", out)
	}
	return m, st
}

func TestTodoWrite(t *testing.T) {
	m, st := invoke(t, `{"todos":[
		{"content":"拆解需求材料","status":"completed"},
		{"content":"批量创建 8 条需求","status":"in_progress","priority":"high"},
		{"content":"生成汇总回复","status":"pending"}]}`)
	if m["ok"] != true {
		t.Fatalf("应成功：%v", m)
	}
	if m["count"].(float64) != 3 || m["completed"].(float64) != 1 {
		t.Fatalf("统计错：%v", m)
	}
	if len(st.items) != 3 {
		t.Fatalf("Store 未写入全量清单：%v", st.items)
	}
	if st.items[0].Priority != "medium" {
		t.Errorf("空 priority 应默认 medium：%v", st.items[0])
	}
}

func TestTodoWriteValidation(t *testing.T) {
	cases := []struct{ name, args string }{
		{"空清单", `{"todos":[]}`},
		{"双进行中", `{"todos":[{"content":"a","status":"in_progress"},{"content":"b","status":"in_progress"}]}`},
		{"坏状态", `{"todos":[{"content":"a","status":"doing"}]}`},
		{"空内容", `{"todos":[{"content":"","status":"pending"}]}`},
		{"坏优先级", `{"todos":[{"content":"a","status":"pending","priority":"urgent"}]}`},
	}
	for _, c := range cases {
		m, st := invoke(t, c.args)
		if m["ok"] != false {
			t.Errorf("%s：应拒绝：%v", c.name, m)
			continue
		}
		if msg, _ := m["error"].(string); len(msg) == 0 {
			t.Errorf("%s：缺错误信息", c.name)
		}
		if len(st.items) != 0 {
			t.Errorf("%s：拒绝时不应写 Store", c.name)
		}
	}
}
