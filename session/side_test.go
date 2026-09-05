package session

// Side 辅助对话回归：记录往返 / 快照继承 / 级联删除。构造语义见
// findings/2026-09-05-zcode-parity-gaps-design.md。

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

func TestParentSIDRecordRoundTrip(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.parentSID = "父会话ID" // 白盒置位（Side 构造前的字段往返验证）
	s.AppendHistory(schema.UserMessage("一"), schema.AssistantMessage("答", nil))
	if s.HistoryLen() != 2 {
		t.Fatalf("HistoryLen：%d", s.HistoryLen())
	}
	reg.Persist(s)

	// 盘面往返：parent_sid 落盘且 Reattach 装载
	reg2 := NewRegistry(st)
	got := reg2.Reattach("张三", s.SID)
	if got == nil || got.ParentOf() != "父会话ID" {
		t.Fatalf("parentSID 应随记录往返：%v", got)
	}
}

func TestSideInheritsSnapshotAndIsolates(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := reg.Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m", Effort: "high"})
	src.AppendHistory(schema.UserMessage("问"), schema.AssistantMessage("答", nil))
	reg.Persist(src)

	side := reg.Side("张三", src.SID)
	if side == nil {
		t.Fatal("Side 应成功")
	}
	if side.ParentOf() != src.SID || side.StateOf() != StateEnded {
		t.Fatalf("side 身份/态：%q %s", side.ParentOf(), side.StateOf())
	}
	if h := side.CloneHistory(); len(h) != 2 || h[0].Content != "问" {
		t.Fatalf("side 应继承父历史快照：%d", len(h))
	}
	if ms := side.ModelSnapshot(); ms.Model != "p/m" || ms.Effort != "high" || side.ModePublic() != "auto" {
		t.Fatal("side 应继承模型偏好与模式")
	}
	// 深隔离：side 追加历史，父零变化；父后续轮不流入 side（快照语义）
	side.AppendHistory(schema.UserMessage("side 问"))
	if len(src.CloneHistory()) != 2 {
		t.Fatal("side 写历史不得波及父")
	}
	src.AppendHistory(schema.UserMessage("父后续"))
	if len(side.CloneHistory()) != 3 {
		t.Fatal("父后续轮不得流入 side（快照语义）")
	}
	// 事件流置空 + 血缘 note（Kind=side，首个自有事件）
	evs := side.SnapshotEvents()
	if len(evs) != 1 {
		t.Fatalf("side 事件流应为空 + 血缘 note：%d", len(evs))
	}
	if d, ok := evs[0].Data.(contract.HarnessNote); !ok || d.Kind != "side" || !strings.Contains(d.Detail, src.SID) {
		t.Fatalf("首事件应为 side 血缘 note：%v", evs[0].Data)
	}
	if side.PendingAppID() != "" {
		t.Fatal("side 不带挂起域")
	}
}

func TestSideAllowsRunningParentAndRejects(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := reg.Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	src.AppendHistory(schema.UserMessage("问"))
	src.SetState(StateRunning) // 核心场景：主任务执行中开 side
	reg.Persist(src)
	if reg.Side("张三", src.SID) == nil {
		t.Fatal("running 父应允许开 side")
	}
	side := reg.Side("张三", src.SID)
	if reg.Side("张三", side.SID) != nil {
		t.Fatal("side of side 应拒绝")
	}
	if reg.Side("李四", src.SID) != nil || reg.Side("张三", "无此会话") != nil {
		t.Fatal("归属不符/未知 sid 应 nil")
	}
}

func TestSidesOfAndCascadeDelete(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := reg.Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	reg.Persist(src)
	s1 := reg.Side("张三", src.SID)
	s2 := reg.Side("张三", src.SID)

	if got := reg.SidesOf("张三", src.SID); len(got) != 2 {
		t.Fatalf("内存+盘面扫 side 应 2 条：%v", got)
	}
	// 盘面路径：新注册表（内存无 side）也可见
	reg2 := NewRegistry(st)
	if got := reg2.SidesOf("张三", src.SID); len(got) != 2 {
		t.Fatalf("盘面扫 side 应 2 条：%v", got)
	}

	reg.Delete("张三", src.SID)
	if got := reg.SidesOf("张三", src.SID); len(got) != 0 {
		t.Fatal("父删除应级联清 side（内存）")
	}
	reg3 := NewRegistry(st)
	if got := reg3.SidesOf("张三", src.SID); len(got) != 0 {
		t.Fatalf("父删除应级联清 side（盘面）：%v", got)
	}
	if _, ok := st.ReadUserTreeFile("张三", "sessions/"+s1.SID+"/session.json"); ok {
		t.Fatal("side 记录树应随父删除")
	}
	_ = s2
}
