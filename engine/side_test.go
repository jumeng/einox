package engine

// Side 引擎侧回归：工作区/外置域父感知（共享寻址）、side 轮末不清工作区、
// SessionBrief.ParentSID 传递。构造语义见 session 包 side_test 与
// findings/2026-09-05 设计文档。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
)

func TestSideWorkspaceSharesParent(t *testing.T) {
	m := newSeamManager(t, nil)
	reg := m.Registry()
	parent := reg.Create("张三", "主任务", "auto", contract.UserPrefs{Model: "p/m"})
	reg.Persist(parent)
	side := reg.Side("张三", parent.SID)
	if side == nil {
		t.Fatal("Side 应成功")
	}

	// ① brief 父身份
	if b := m.briefOf(side); b.ParentSID != parent.SID {
		t.Fatalf("brief 应带父身份：%q", b.ParentSID)
	}
	if b := m.briefOf(parent); b.ParentSID != "" {
		t.Fatal("普通会话 brief.ParentSID 应为空")
	}

	// ② 会话域文件面读父工作区：父工作区放文件，side 的 read_file 能读
	ws := m.workspaceOf(parent)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "现场.txt"), []byte("父在途现场"), 0o644); err != nil {
		t.Fatal(err)
	}
	sts, err := m.sessionTools(side)
	if err != nil {
		t.Fatalf("sessionTools：%v", err)
	}
	var rf contract.Tool
	for _, t2 := range sts {
		if t2.Info().Name == "read_file" {
			rf = t2
		}
	}
	if rf == nil {
		t.Fatal("应有 read_file")
	}
	res, err := rf.Invoke(context.Background(), json.RawMessage(`{"path":"现场.txt"}`))
	if err != nil || !strings.Contains(string(res), "父在途现场") {
		t.Fatalf("side 应能读父工作区：%s %v", res, err)
	}

	// ③ side 轮末不清共享工作区；父轮末清自己的
	m.wipeWorkspace(side)
	if _, err := os.Stat(filepath.Join(ws, "现场.txt")); err != nil {
		t.Fatal("side 收尾不得清父工作区")
	}
	m.wipeWorkspace(parent)
	if _, err := os.Stat(filepath.Join(ws, "现场.txt")); err == nil {
		t.Fatal("父收尾应清自己的工作区")
	}
}

func TestSideSpillResolvesParentDir(t *testing.T) {
	m := newSeamManager(t, nil)
	reg := m.Registry()
	parent := reg.Create("张三", "主任务", "auto", contract.UserPrefs{Model: "p/m"})
	reg.Persist(parent)
	side := reg.Side("张三", parent.SID)

	// spill 取回根 = 父会话 spill 目录（side 无复制、直读共享域）
	if got, want := m.spillDirOf(side), m.spillDirOf(parent); got != want {
		t.Fatalf("side spill 应解析到父目录：%s != %s", got, want)
	}
}

// TestGateCheckerRootParentAware FinalGate 判据的工作区根父感知：side 的
// 工具面工作在父工作区，判据拿到的根必须是父根（否则门对 side 形同虚设）。
func TestGateCheckerRootParentAware(t *testing.T) {
	m := newSeamManager(t, nil)
	reg := m.Registry()
	parent := reg.Create("张三", "主任务", "auto", contract.UserPrefs{Model: "p/m"})
	reg.Persist(parent)
	side := reg.Side("张三", parent.SID)

	var got string
	if err := m.runGateCheckers(context.Background(), side, &GateConfig{
		Checkers: []GateChecker{func(_ context.Context, root string) error { got = root; return nil }},
	}); err != nil {
		t.Fatalf("runGateCheckers：%v", err)
	}
	if want := m.Opt.WorkspaceRoot("张三", parent.SID); got != want {
		t.Fatalf("side 的门判据根应为父工作区：%s != %s", got, want)
	}
}

// TestTranscriptSideWritesParentNoCollision side 压缩 transcript 的读写对称：
// 存储域走父 spill 目录（wsSID 共享寻址），文件名带自身 SID 防与父固定名
// 互相覆盖；side 的 read_file 能读回自己那份（通知卡承诺可溯源）。
func TestTranscriptSideWritesParentNoCollision(t *testing.T) {
	m := newSeamManager(t, nil)
	reg := m.Registry()
	parent := reg.Create("张三", "主任务", "auto", contract.UserPrefs{Model: "p/m"})
	reg.Persist(parent)
	side := reg.Side("张三", parent.SID)

	msgs := []*schema.Message{schema.UserMessage("压缩前全文")}
	writeTranscript(m.reg.Store(), parent, msgs)
	writeTranscript(m.reg.Store(), side, msgs)

	// 同目录共存不覆盖：sessions/<父SID>/spill/ 下两份 SID 命名文件
	st := m.reg.Store()
	if _, ok := st.ReadUserTreeFile("张三", "sessions/"+parent.SID+"/spill/transcript-"+parent.SID+".txt"); !ok {
		t.Fatal("父 transcript 应以自身 SID 命名落在自身目录")
	}
	if _, ok := st.ReadUserTreeFile("张三", "sessions/"+parent.SID+"/spill/transcript-"+side.SID+".txt"); !ok {
		t.Fatal("side transcript 应以自身 SID 命名落在父目录（共享寻址）")
	}

	// side 的 read_file 读回自己那份（虚拟前缀解析到父目录）
	sts, err := m.sessionTools(side)
	if err != nil {
		t.Fatalf("sessionTools：%v", err)
	}
	var rf contract.Tool
	for _, t2 := range sts {
		if t2.Info().Name == "read_file" {
			rf = t2
		}
	}
	res, err := rf.Invoke(context.Background(), json.RawMessage(`{"path":"spill/transcript-`+side.SID+`.txt"}`))
	if err != nil || !strings.Contains(string(res), "压缩前全文") {
		t.Fatalf("side 应能经 read_file 溯源自身 transcript：%s %v", res, err)
	}
}
