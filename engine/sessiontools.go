package engine

// 会话域工具面装配（自产品 internal/agent/extwire.go 迁入）：需要会话态的
// 基座件在此挂接——todo 的清单写 = 落会话事件流（Record 即扇出：活跃视图
// 实时收到 todo_update，回放完整可见）；ask_user 与 submit_plan 的挂起/续流
// 复用审批通道（Suspend → pump 转事件卡 → answer/approve 端点 Resume；
// 计划文档落用户域 plans/<sid>/）。工作区件（fsutil/runcommand/applypatch）
// root = 会话工作区（WorkspaceRoot 注入，用户域 workspaces/<sid>）：
// WorkspaceProtect 注入 fsutil/applypatch 写面（写保护区），WorkspaceKeep
// 声明持久子区（收尾清理豁免）。生命周期主锚点 = 任务正常收尾即清，挂起/
// 异常态保留待续。

import (
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
	"github.com/jumeng/einox/tools/applypatch"
	"github.com/jumeng/einox/tools/askuser"
	"github.com/jumeng/einox/tools/fsutil"
	"github.com/jumeng/einox/tools/plan"
	"github.com/jumeng/einox/tools/runcommand"
	"github.com/jumeng/einox/tools/todo"
	"github.com/jumeng/einox/workspace"
)

// sessionTodos 会话域 TodoStore 实现。
type sessionTodos struct{ s *session.Session }

func (t sessionTodos) Set(items []todo.Item) {
	t.s.Record(contract.EvTodoUpdate, items)
}

// sessionAskResolver ask_user 决议读取（Resume 时 askuser 消费）。
type sessionAskResolver struct{ s *session.Session }

func (r sessionAskResolver) Decision() *askuser.Decision {
	if d := r.s.TakeAskDecision(); d != nil {
		return &askuser.Decision{Answers: d.Answers, FreeText: d.FreeText}
	}
	return nil
}

// sessionPlanSrc submit_plan 会话域依赖（决议消费/任务授权/档位/序号取号）。
type sessionPlanSrc struct{ s *session.Session }

func (p sessionPlanSrc) TakeDecision() *plan.Decision { return p.s.TakeDecision() }
func (p sessionPlanSrc) GrantTask()                   { p.s.GrantTask() }
func (p sessionPlanSrc) ModePublic() string           { return p.s.ModePublic() }
func (p sessionPlanSrc) NextPlanSeq() int             { return p.s.NextPlanSeq() }

// 会话域工具族名（Options.SessionToolsOff 的封闭取值集——NewManager 期校验）。
const (
	FamilyTodo  = "todo"  // todo_write（任务清单）
	FamilyAsk   = "ask"   // ask_user（结构化提问）
	FamilyPlan  = "plan"  // submit_plan（计划卡）
	FamilyFS    = "fs"    // read_file / list_dir / search_files / delete_file
	FamilyCmd   = "cmd"   // run_command / task_output / task_stop
	FamilyPatch = "patch" // apply_patch
)

// familyOff 族是否被 SessionToolsOff 裁掉（名单已经 NewManager 校验，线性查够用）。
func familyOff(off []string, family string) bool {
	for _, f := range off {
		if f == family {
			return true
		}
	}
	return false
}

// sessionTools 会话域工具面（组装期追加，见 assemble）。SessionToolsOff 按
// 族裁剪（nil/空 = 全挂零行为变化）；各族构造错误上抛（assemble 转 CONFIG
// 错误事件）——族无声缺席是装配错误，不静默。
func (m *Manager) sessionTools(s *session.Session) ([]contract.Tool, error) {
	off := m.Opt.SessionToolsOff
	ws := m.workspaceOf(s)
	var out []contract.Tool
	if !familyOff(off, FamilyTodo) {
		ts, err := todo.NewTools(todo.Config{Store: sessionTodos{s}})
		if err != nil {
			return nil, &configError{"todo 工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	if !familyOff(off, FamilyAsk) {
		ts, err := askuser.NewTools(askuser.Config{Resolver: sessionAskResolver{s}})
		if err != nil {
			return nil, &configError{"ask_user 工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	if !familyOff(off, FamilyPlan) {
		ts, err := plan.NewTools(plan.Config{
			S:   sessionPlanSrc{s},
			SID: s.SID,
			Writer: func(rel string, data []byte) error { // 用户域文件面（不走 DATA_DIR 数据门禁——计划是过程态）
				return m.reg.Store().WriteUserTreeFile(s.Owner, rel, data)
			},
		})
		if err != nil {
			return nil, &configError{"submit_plan 工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	if !familyOff(off, FamilyFS) {
		ts, err := fsutil.NewTools(fsutil.Config{Root: ws, Spill: m.spillDirOf(s), ProtectDirs: m.Opt.WorkspaceProtect})
		if err != nil {
			return nil, &configError{"文件面工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	if !familyOff(off, FamilyCmd) {
		ts, err := runcommand.NewTools(runcommand.Config{Root: ws, Sandbox: m.Opt.Sandbox, SandboxProvider: m.Opt.SandboxProvider, Egress: m.Opt.Egress})
		if err != nil {
			return nil, &configError{"命令工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	if !familyOff(off, FamilyPatch) {
		ts, err := applypatch.NewTools(applypatch.Config{Root: ws, ProtectDirs: m.Opt.WorkspaceProtect})
		if err != nil {
			return nil, &configError{"apply_patch 工具族构造失败：" + err.Error()}
		}
		out = append(out, ts...)
	}
	return out, nil
}

// wsSIDOf 工作区/外置域寻址键（包级——writeTranscript 等非 Manager 件共用）：
// 辅助对话共享父会话域（ZCode「同一任务上下文」语义——side 能读父当轮产出
// 与 spill 外置域）。父轮末即清对 side 的可见性影响是共享临时域的固有代价
// （工作区一轮一清，side 的文件上下文跟随父任务）。
func wsSIDOf(s *session.Session) string {
	if p := s.ParentOf(); p != "" {
		return p
	}
	return s.SID
}

// wsSID 同 wsSIDOf（Manager 方法形态——workspaceOf/wipeWorkspace/spillDirOf 寻址）。
func (m *Manager) wsSID(s *session.Session) string { return wsSIDOf(s) }

// workspaceOf 会话工作区（WorkspaceRoot 注入，用户域 workspaces/<sid>；
// 模型见 einox/workspace）。
func (m *Manager) workspaceOf(s *session.Session) string {
	return workspace.Of(m.Opt.WorkspaceRoot(s.Owner, m.wsSID(s)))
}

// wipeWorkspace 任务正常收尾即清会话工作区（一轮任务一清；WorkspaceKeep
// 声明的持久子区豁免，随会话删除/过期/孤儿清扫整清）。辅助对话跳过——
// 共享工作区的清理权在父会话（side 收尾清了会毁父在途现场）。
func (m *Manager) wipeWorkspace(s *session.Session) {
	if s.ParentOf() != "" {
		return
	}
	workspace.Wipe(m.Opt.WorkspaceRoot(s.Owner, m.wsSID(s)), m.Opt.WorkspaceKeep...)
}
