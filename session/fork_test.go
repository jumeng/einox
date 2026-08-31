package session

// Fork 全量快照分叉回归（B8）：深隔离（record JSON 往返零共享指针）/ 事件 ID
// 接续 / 挂起域不带 / 血缘 note / spill 复制 / V1 禁源 running / 归属不符 /
// 磁盘重建路径。

import (
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// forkFixture 造一个有事件+历史+spill 的源会话并落盘。
func forkFixture(t *testing.T, reg *Registry, st *tstore.Store) *Session {
	t.Helper()
	s := reg.Create("张三", "原任务", "auto", contract.UserPrefs{Model: "p/m"})
	s.Record("user_message", contract.UserMsg{Text: "问题一"})
	s.Record("text_delta", contract.Delta{Delta: "答复"})
	s.AppendHistory(schema.UserMessage("问题一"), schema.AssistantMessage("答复", nil))
	if err := st.WriteUserTreeFile("张三", "sessions/"+s.SID+"/spill/tool/abc", []byte("超长工具结果全文")); err != nil {
		t.Fatalf("写 spill：%v", err)
	}
	reg.Persist(s)
	return s
}

func TestForkDeepIsolationAndLineage(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := forkFixture(t, reg, st)
	srcEvents := len(src.SnapshotEvents())

	ns := reg.Fork("张三", src.SID)
	if ns == nil {
		t.Fatal("分叉应成功")
	}
	if ns.SID == src.SID {
		t.Fatal("分叉体应有新 SID")
	}
	// 深隔离：分叉体追加历史/事件，源零变化
	ns.AppendHistory(schema.UserMessage("分叉后输入"))
	ns.Record("user_message", contract.UserMsg{Text: "分叉后输入"})
	if len(src.CloneHistory()) != 2 {
		t.Fatal("分叉体写历史不得波及源（深拷贝）")
	}
	if len(src.SnapshotEvents()) != srcEvents {
		t.Fatal("源事件流零增量")
	}
	// 事件 ID 接续：分叉体新事件 ID = 末位+1（fork note 之后的下一条）
	evs := ns.SnapshotEvents()
	last := evs[len(evs)-1]
	if last.ID <= srcEvents {
		t.Fatalf("分叉体事件 ID 应接续末位：%d", last.ID)
	}
	// 血缘 note：分叉体首个自有事件，Detail 含源 sid
	forkNoteIdx := -1
	for i, ev := range evs {
		if ev.Event == contract.EvHarnessNote {
			if d, ok := ev.Data.(contract.HarnessNote); ok && d.Kind == "fork" {
				forkNoteIdx = i
				if !strings.Contains(d.Detail, src.SID) {
					t.Fatalf("血缘 note 应含源 sid：%s", d.Detail)
				}
			}
		}
	}
	if forkNoteIdx != srcEvents { // 复制事件之后的第一条（复制事件 ID 1..srcEvents）
		t.Fatalf("血缘 note 应为分叉体首个自有事件：位 %d（源事件 %d 条）", forkNoteIdx, srcEvents)
	}
	// 历史携带：续聊回传从分叉点继续
	if len(ns.CloneHistory()) != 3 { // 源 2 条 + 分叉后输入 1 条
		t.Fatalf("分叉体历史应携带源消息：%d 条", len(ns.CloneHistory()))
	}
	// 落盘可恢复：Reattach(分叉体) 完整
	reg2 := NewRegistry(st)
	if got := reg2.Reattach("张三", ns.SID); got == nil {
		t.Fatal("分叉体应已落盘可 Reattach")
	}
	// spill 复制：分叉体 read_file 虚拟前缀指向自身目录
	if data, ok := st.ReadUserTreeFile("张三", "sessions/"+ns.SID+"/spill/tool/abc"); !ok || string(data) != "超长工具结果全文" {
		t.Fatal("spill 外置域应整目录复制到分叉体")
	}
}

func TestForkPendingSourceClearsSuspendDomain(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := reg.Create("张三", "挂起源", "manual", contract.UserPrefs{Model: "p/m"})
	src.SetPendingApproval("a123456")
	src.SetPendingDue("approval", time.Now().Add(time.Hour))
	reg.Persist(src)

	ns := reg.Fork("张三", src.SID)
	if ns == nil {
		t.Fatal("挂起态源分叉应允许（V1 仅禁 running）")
	}
	if ns.StateOf() != StateEnded || ns.PendingAppID() != "" {
		t.Fatalf("分叉体应无僵尸挂起态：state=%s pending=%q", ns.StateOf(), ns.PendingAppID())
	}
}

func TestForkRejectsRunningAndForeignOwner(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := forkFixture(t, reg, st)

	src.SetState(StateRunning)
	if reg.Fork("张三", src.SID) != nil {
		t.Fatal("源 running 时分叉应返回 nil（V1 禁令）")
	}
	src.SetState(StateEnded)
	if reg.Fork("李四", src.SID) != nil {
		t.Fatal("归属不符应返回 nil")
	}
	if reg.Fork("张三", "s不存在") != nil {
		t.Fatal("未知 sid 应返回 nil")
	}
}

func TestForkFromDisk(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	src := forkFixture(t, reg, st)
	srcSID := src.SID

	// 新注册表（内存无此会话）→ 磁盘重建路径分叉
	reg2 := NewRegistry(st)
	ns := reg2.Fork("张三", srcSID)
	if ns == nil {
		t.Fatal("磁盘重建路径分叉应成功（历史会话分叉是主场景）")
	}
	if len(ns.CloneHistory()) != 2 || ns.CloneHistory()[0].Content != "问题一" {
		t.Fatalf("磁盘路径应携带源历史：%d 条", len(ns.CloneHistory()))
	}
	// 内存路径与磁盘路径产物一致：血缘 note 在
	found := false
	for _, ev := range ns.SnapshotEvents() {
		if ev.Event == contract.EvHarnessNote {
			if d, ok := ev.Data.(contract.HarnessNote); ok && d.Kind == "fork" && strings.Contains(d.Detail, srcSID) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("磁盘路径分叉体应含血缘 note")
	}
}
