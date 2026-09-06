package session

// T6 名册继承矩阵回归（第三轮审查盲区补齐）：participants 是会话域字段，
// Fork/ForkAt/Side/Reattach 四路必须一致携带（四处手抄拷贝无护栏——本测试
// 即护栏），且派生会话名册可独立演化（改派生不改父）。附带 EventsSince
// （订阅满即弃的补投源——渠道泵与 ui SSE 共用面）。

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

func rosterHas(s *Session, ids ...string) bool {
	have := map[string]bool{}
	for _, p := range s.ParticipantsOf() {
		have[p.ID] = true
	}
	for _, id := range ids {
		if !have[id] {
			return false
		}
	}
	return true
}

func TestParticipantsInheritance(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := reg.Create("张三", "群任务", "plan", contract.UserPrefs{Model: "p/m"})
	src.UpsertParticipant(contract.Participant{ID: "u_zhang", Name: "张三"})
	src.UpsertParticipant(contract.Participant{ID: "u_li", Name: "李四"})
	// ForkAt 锚：一轮完整会话（HistLen=2）
	src.AppendHistory(schema.UserMessage("问"), schema.AssistantMessage("答", nil))
	anchor := src.Record(contract.EvSessionEnd, contract.SessionEnd{HistLen: 2})
	reg.Persist(src)

	fork := reg.Fork("张三", src.SID)
	if fork == nil {
		t.Fatal("Fork 应成功")
	}
	if !rosterHas(fork, "u_zhang", "u_li") {
		t.Fatalf("Fork 应继承名册：%v", fork.ParticipantsOf())
	}
	forkAt := reg.ForkAt("张三", src.SID, anchor.ID)
	if forkAt == nil {
		t.Fatal("ForkAt 应成功")
	}
	if !rosterHas(forkAt, "u_zhang", "u_li") {
		t.Fatalf("ForkAt 应继承名册：%v", forkAt.ParticipantsOf())
	}
	side := reg.Side("张三", src.SID)
	if side == nil {
		t.Fatal("Side 应成功")
	}
	if !rosterHas(side, "u_zhang", "u_li") {
		t.Fatalf("Side 应继承名册（同域辅助对话）：%v", side.ParticipantsOf())
	}
	// 重启续接（盘面 → 新 Registry）
	reg2 := NewRegistry(st)
	ra := reg2.Reattach("张三", src.SID)
	if ra == nil {
		t.Fatal("Reattach 应续接")
	}
	if !rosterHas(ra, "u_zhang", "u_li") {
		t.Fatalf("Reattach 应恢复名册（session.json 续接）：%v", ra.ParticipantsOf())
	}
	// 派生独立演化：fork 登记 王五 不影响父与兄弟
	fork.UpsertParticipant(contract.Participant{ID: "u_wang", Name: "王五"})
	if rosterHas(src, "u_wang") || rosterHas(side, "u_wang") {
		t.Fatal("派生会话名册应独立演化（登记不回写父/兄弟）")
	}
	// 父登记也不影响已分叉的派生（快照语义）
	src.UpsertParticipant(contract.Participant{ID: "u_zhao", Name: "赵六"})
	if rosterHas(fork, "u_zhao") {
		t.Fatal("分叉后父名册变更不应追改派生（快照固化）")
	}
}

func TestEventsSince(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("张三", "事件", "plan", contract.UserPrefs{})
	for i := 0; i < 5; i++ {
		s.Record(contract.EvTextDelta, contract.Delta{Delta: "x"})
	}
	got := s.EventsSince(3)
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 5 {
		t.Fatalf("EventsSince(3) 应得事件 4、5，实得 %+v", got)
	}
	if all := s.EventsSince(0); len(all) != 5 {
		t.Fatalf("EventsSince(0) 应全量，实得 %d", len(all))
	}
	if none := s.EventsSince(5); len(none) != 0 {
		t.Fatalf("水位即末位应得空，实得 %d", len(none))
	}
}
