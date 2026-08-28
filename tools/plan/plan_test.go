package plan

// submit_plan 行为回归（findings/2026-08-25 定稿语义）：校验拒空/限长；
// auto 档写完即走不挂起；plan/manual 档挂起（Suspend 载 PlanCard）；恢复流
// 三分叉——批准（plan 档授权任务期 GrantTask / manual 档不授权）、拒绝
//（原因回喂）、无决议（fail-closed 作废）。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jumeng/einox/contract"
)

// fakeSession 会话域桩（决议/授权/档位/序号）。
type fakeSession struct {
	mu        sync.Mutex
	decision  *Decision
	taskGrant bool
	mode      string
	seq       int
}

func (f *fakeSession) TakeDecision() *Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.decision
	f.decision = nil
	return d
}
func (f *fakeSession) GrantTask() { f.mu.Lock(); f.taskGrant = true; f.mu.Unlock() }
func (f *fakeSession) TaskGranted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.taskGrant
}
func (f *fakeSession) ModePublic() string { return f.mode }
func (f *fakeSession) NextPlanSeq() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return f.seq
}

// memWriter 文档写入桩（记录路径→内容）。
type memWriter struct {
	mu   sync.Mutex
	docs map[string][]byte
}

func (w *memWriter) write(rel string, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.docs == nil {
		w.docs = map[string][]byte{}
	}
	w.docs[rel] = data
	return nil
}

func validIn() planIn {
	return planIn{
		Task: "收集全部项目进度并更新周报", Summary: "先采集三版本 git 与 issue 进度，分类后写 W35 周报",
		Steps: []stepIn{{Title: "采集", Detail: "fetch_and_collect 三版本"}, {Title: "写周报", Detail: "write_weekly_report"}},
	}
}

func mustTools(t *testing.T, s *fakeSession, w *memWriter) []contract.Tool {
	t.Helper()
	ts, err := NewTools(Config{S: s, SID: "stest01", Writer: w.write})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func invoke(t *testing.T, ts []contract.Tool, ctx context.Context, args string) (map[string]any, error) {
	t.Helper()
	raw, err := ts[0].Invoke(ctx, json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		t.Fatalf("结果应为 JSON：%s", raw)
	}
	return out, nil
}

func argsOf(t *testing.T, in planIn) string {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestValidateRejects(t *testing.T) {
	s := &fakeSession{mode: "plan"}
	ts := mustTools(t, s, &memWriter{})
	for name, in := range map[string]planIn{
		"空 task":    {Summary: "s", Steps: []stepIn{{Title: "a"}}},
		"空 summary": {Task: "t", Steps: []stepIn{{Title: "a"}}},
		"无步骤":       {Task: "t", Summary: "s"},
		"空步骤名":      {Task: "t", Summary: "s", Steps: []stepIn{{Title: " "}}},
	} {
		out, err := invoke(t, ts, context.Background(), argsOf(t, in))
		if err != nil || out["ok"] != false {
			t.Errorf("%s 应校验拒绝：%v err=%v", name, out, err)
		}
	}
}

func TestAutoModeWritesDocAndPasses(t *testing.T) {
	s := &fakeSession{mode: "auto"}
	w := &memWriter{}
	ts := mustTools(t, s, w)
	out, err := invoke(t, ts, context.Background(), argsOf(t, validIn()))
	if err != nil {
		t.Fatalf("auto 不应挂起：%v", err)
	}
	if out["ok"] != true || out["path"] != "plans/stest01/1.md" {
		t.Fatalf("auto 应写档即走：%v", out)
	}
	doc := string(w.docs["plans/stest01/1.md"])
	if !strings.Contains(doc, "auto（不送审）") || !strings.Contains(doc, validIn().Task) {
		t.Fatalf("auto 文档状态/内容异常：%s", doc)
	}
}

func TestPlanModeSuspendsWithCard(t *testing.T) {
	s := &fakeSession{mode: "plan"}
	w := &memWriter{}
	ts := mustTools(t, s, w)
	_, err := invoke(t, ts, context.Background(), argsOf(t, validIn()))
	su, ok := err.(*contract.Suspend)
	if !ok {
		t.Fatalf("plan 档应挂起，得到：%v", err)
	}
	card, ok := su.Info.(contract.PlanCard)
	if !ok || card.Task != validIn().Task || card.Seq != 1 || card.Path != "plans/stest01/1.md" || card.Mode != "plan" {
		t.Fatalf("挂起载荷应为计划卡：%+v", su.Info)
	}
	if !strings.Contains(string(w.docs[card.Path]), "submitted") {
		t.Fatal("挂起前文档应已落盘（submitted）")
	}
}

func TestResumeDecisions(t *testing.T) {
	// 批准（plan 档）：授权任务期 + 文档 approved + 成功回执
	s := &fakeSession{mode: "plan"}
	w := &memWriter{}
	ts := mustTools(t, s, w)
	_, err := invoke(t, ts, context.Background(), argsOf(t, validIn()))
	if _, ok := err.(*contract.Suspend); !ok {
		t.Fatalf("应挂起：%v", err)
	}
	s.decision = &Decision{Approve: true}
	ctx := contract.WithResumeState(context.Background(), planState{Info: contract.PlanCard{
		Task: "t", Seq: 1, Path: "plans/stest01/1.md", Mode: "plan",
	}})
	out, err := invoke(t, ts, ctx, argsOf(t, validIn()))
	if err != nil || out["approved"] != true || !s.TaskGranted() {
		t.Fatalf("plan 批准应授权任务期：%v err=%v granted=%v", out, err, s.TaskGranted())
	}
	if !strings.Contains(string(w.docs["plans/stest01/1.md"]), "approved") {
		t.Fatal("批准后文档应翻 approved")
	}

	// 批准（manual 档）：确认方向但不授权
	s2 := &fakeSession{mode: "manual"}
	w2 := &memWriter{}
	ts2 := mustTools(t, s2, w2)
	s2.decision = &Decision{Approve: true}
	out, err = invoke(t, ts2, contract.WithResumeState(context.Background(), planState{Info: contract.PlanCard{
		Task: "t", Seq: 1, Path: "plans/stest01/1.md", Mode: "manual",
	}}), argsOf(t, validIn()))
	if err != nil || out["approved"] != true || s2.TaskGranted() {
		t.Fatalf("manual 批准不应授权：%v err=%v", out, err)
	}

	// 拒绝：原因回喂 + 文档 rejected
	s3 := &fakeSession{mode: "plan"}
	w3 := &memWriter{}
	ts3 := mustTools(t, s3, w3)
	s3.decision = &Decision{Approve: false, Reason: "范围太大"}
	out, err = invoke(t, ts3, contract.WithResumeState(context.Background(), planState{Info: contract.PlanCard{
		Task: "t", Seq: 1, Path: "plans/stest01/1.md", Mode: "plan",
	}}), argsOf(t, validIn()))
	if err != nil || out["ok"] != false || !strings.Contains(out["error"].(string), "范围太大") || s3.TaskGranted() {
		t.Fatalf("拒绝应回喂原因且不授权：%v err=%v", out, err)
	}
	if !strings.Contains(string(w3.docs["plans/stest01/1.md"]), "rejected") {
		t.Fatal("拒绝后文档应翻 rejected")
	}

	// 无决议：fail-closed 作废
	s4 := &fakeSession{mode: "plan"}
	ts4 := mustTools(t, s4, &memWriter{})
	out, err = invoke(t, ts4, contract.WithResumeState(context.Background(), planState{Info: contract.PlanCard{
		Task: "t", Seq: 1, Path: "plans/stest01/1.md", Mode: "plan",
	}}), argsOf(t, validIn()))
	if err != nil || out["ok"] != false || s4.TaskGranted() {
		t.Fatalf("无决议应 fail-closed 作废：%v err=%v", out, err)
	}
}

func TestSeqIncrements(t *testing.T) {
	s := &fakeSession{mode: "plan"}
	w := &memWriter{}
	ts := mustTools(t, s, w)
	for i := 0; i < 3; i++ {
		if _, err := ts[0].Invoke(context.Background(), json.RawMessage(argsOf(t, validIn()))); err == nil {
			t.Fatal("plan 档应挂起")
		}
	}
	if s.seq != 3 {
		t.Fatalf("序号应递增到 3：%d", s.seq)
	}
	if _, ok := w.docs["plans/stest01/3.md"]; !ok {
		t.Fatal("修订文档应按新序号落盘")
	}
}
