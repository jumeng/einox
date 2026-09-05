package session

// ForkAt 锚定分叉回归：截断正确性 / 未知锚与零值锚拒绝 / side 拒分叉 /
// anchor=0 与 Fork 等价 / 盘面路径。设计见 findings/2026-09-05。

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// forkAnchorFixture 两轮会话：轮 1（锚）与轮 2（锚后）。事件序列含
// user_message/text_delta/session_end(HistLen)；fileChanges 经 RecordFileChange
// 与 session_end.Files 载荷双轨落账。
func forkAnchorFixture(t *testing.T, reg *Registry, st *tstore.Store) (*Session, int, int) {
	t.Helper()
	s := reg.Create("张三", "任务", "auto", contract.UserPrefs{Model: "p/m"})
	// 轮 1
	s.Record("user_message", contract.UserMsg{Text: "问一"})
	s.Record("text_delta", contract.Delta{Delta: "答一"})
	s.AppendHistory(schema.UserMessage("问一"), schema.AssistantMessage("答一", nil))
	s.RecordFileChange("a.txt", "create")
	anchorEv := s.Record(contract.EvSessionEnd, contract.SessionEnd{
		Summary: "答一", HistLen: 2,
		Files: []contract.FileChange{{Path: "a.txt", Action: "create", Count: 1}},
	})
	// 轮 2
	s.Record("user_message", contract.UserMsg{Text: "问二"})
	s.Record("text_delta", contract.Delta{Delta: "答二"})
	s.AppendHistory(schema.UserMessage("问二"), schema.AssistantMessage("答二", nil))
	s.RecordFileChange("b.txt", "edit")
	s.RecordFileChange("b.txt", "edit")
	lastEv := s.Record(contract.EvSessionEnd, contract.SessionEnd{
		Summary: "答二", HistLen: 4,
		Files: []contract.FileChange{
			{Path: "a.txt", Action: "create", Count: 1},
			{Path: "b.txt", Action: "edit", Count: 2},
		},
	})
	reg.Persist(s)
	return s, anchorEv.ID, lastEv.ID
}

func TestForkAtTruncatesAtAnchor(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src, anchor, _ := forkAnchorFixture(t, reg, st)

	ns := reg.ForkAt("张三", src.SID, anchor)
	if ns == nil {
		t.Fatal("锚定分叉应成功")
	}
	if evs := ns.SnapshotEvents(); len(evs) != anchor+1 || evs[len(evs)-2].ID != anchor {
		t.Fatalf("事件应截至锚（含）+ 血缘 note：n=%d", len(evs))
	}
	if h := ns.CloneHistory(); len(h) != 2 || h[1].Content != "答一" {
		t.Fatalf("历史应截至锚 HistLen：%d", len(h))
	}
	if got := ns.FileChangesSnapshot(); len(got) != 1 || got[0].Path != "a.txt" || got[0].Count != 1 {
		t.Fatalf("fileChanges 应取锚事件载荷：%v", got)
	}
	if ns.SummaryOf() != "答一" {
		t.Fatalf("summary 应取锚事件载荷：%q", ns.SummaryOf())
	}
	// 后续事件接续：新事件 ID > 锚
	ev := ns.Record("user_message", contract.UserMsg{Text: "岔出"})
	if ev.ID <= anchor {
		t.Fatalf("新事件应接续锚 ID：%d", ev.ID)
	}
	// 源零增量
	if len(src.CloneHistory()) != 4 {
		t.Fatal("源历史不得被截断波及")
	}
}

func TestForkAtRejectsBadAnchors(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src, anchor, _ := forkAnchorFixture(t, reg, st)

	if reg.ForkAt("张三", src.SID, 99999) != nil {
		t.Fatal("不存在的事件 ID 应 nil")
	}
	if reg.ForkAt("张三", src.SID, -1) != nil {
		t.Fatal("负锚应 nil（fail-closed——非 0 即要求合法锚）")
	}
	if reg.ForkAt("张三", src.SID, 1) != nil {
		t.Fatal("非 session_end 事件应 nil")
	}
	if reg.ForkAt("李四", src.SID, anchor) != nil {
		t.Fatal("归属不符应 nil")
	}
	// 旧零值锚：HistLen=0 的 session_end 不可锚
	zero := reg.Create("张三", "旧会话", "auto", contract.UserPrefs{Model: "p/m"})
	zeroEv := zero.Record(contract.EvSessionEnd, contract.SessionEnd{Summary: "旧"})
	reg.Persist(zero)
	if reg.ForkAt("张三", zero.SID, zeroEv.ID) != nil {
		t.Fatal("零值 HistLen 的 session_end 不可锚（旧存量兼容）")
	}
}

func TestForkAtZeroEqualsFork(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src, _, _ := forkAnchorFixture(t, reg, st)

	ns := reg.ForkAt("张三", src.SID, 0)
	if ns == nil || len(ns.CloneHistory()) != 4 || len(ns.SnapshotEvents()) != len(src.SnapshotEvents())+1 {
		t.Fatal("anchor=0 应与 Fork 等价（全量 + 血缘 note）")
	}
}

func TestForkAtFromDiskAndRejectsSide(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src, anchor, _ := forkAnchorFixture(t, reg, st)

	// 盘面路径（Data 为 map 形态——JSON 往返还原）
	reg2 := NewRegistry(st)
	ns := reg2.ForkAt("张三", src.SID, anchor)
	if ns == nil || len(ns.CloneHistory()) != 2 {
		t.Fatal("盘面路径锚定分叉应成功且截断正确")
	}
	// side 不可分叉（ParentSID 置位）
	side := reg.Create("张三", "side", "auto", contract.UserPrefs{Model: "p/m"})
	side.parentSID = src.SID
	reg.Persist(side)
	if reg.Fork("张三", side.SID) != nil || reg.ForkAt("张三", side.SID, 0) != nil {
		t.Fatal("side 会话不可分叉")
	}
	// 盘面路径同样拒绝 side
	reg3 := NewRegistry(st)
	if reg3.Fork("张三", side.SID) != nil {
		t.Fatal("盘面路径 side 不可分叉")
	}
}
