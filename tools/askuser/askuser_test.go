package askuser

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jumeng/einox/contract"
)

type fixedResolver struct{ d *Decision }

func (f fixedResolver) Decision() *Decision { return f.d }

func invoke(t *testing.T, args string) (map[string]any, error) {
	t.Helper()
	ts, err := NewTools(Config{Resolver: fixedResolver{}})
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	out, err := ts[0].Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if json.Unmarshal(out, &m) != nil {
		t.Fatalf("非 JSON 输出：%s", out)
	}
	return m, nil
}

func TestAskUserValidation(t *testing.T) {
	cases := []struct{ name, args, wantErr string }{
		{"空问题", `{"question":"  "}`, "question 不能为空"},
		{"无选项无自由文本", `{"question":"哪个？"}`, "allow_free_text"},
		{"选项过多", `{"question":"?","options":[{"label":"1"},{"label":"2"},{"label":"3"},{"label":"4"},{"label":"5"},{"label":"6"},{"label":"7"}]}`, "最多 6 个"},
		{"空label", `{"question":"?","options":[{"label":" "}]}`, "label 为空"},
	}
	for _, c := range cases {
		m, err := invoke(t, c.args)
		if err != nil {
			t.Errorf("%s：不应上抛错误：%v", c.name, err)
			continue
		}
		if m["ok"] != false {
			t.Errorf("%s：应拒绝：%v", c.name, m)
		}
	}
}

func TestAskUserSuspend(t *testing.T) {
	// 有效提问 → Suspend 哨兵上抛（适配层转引擎中断挂起）
	ts, _ := NewTools(Config{Resolver: fixedResolver{}})
	_, err := ts[0].Invoke(context.Background(), json.RawMessage(
		`{"question":"周报封面用哪个版本？","options":[{"label":"v2.8 正式版"},{"label":"v2.9 预览版"}]}`))
	var su *contract.Suspend
	if !errors.As(err, &su) {
		t.Fatalf("有效提问应返回挂起哨兵，得到：%v", err)
	}
	card, ok := su.Info.(contract.AskCard)
	if !ok || card.Question == "" || len(card.Options) != 2 {
		t.Errorf("挂起载荷应为提问卡：%v", su.Info)
	}
}
