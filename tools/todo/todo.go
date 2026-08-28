// Package todo 提供 todo_write 任务清单工具：多步任务的自跟踪与用户可见
// 进度。全量覆盖写语义（对齐 Claude Code TodoWrite——模型不易漂移；增量
// 语义模型常漏清旧项）；schema 参照 deepseek-harness packages/todo（MIT）。
// 事件化由装配层经 Store 落会话事件流（实时扇出 + 回放可见）。
package todo

import (
	"context"
	"strconv"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Item 清单条目。
type Item struct {
	Content  string `json:"content"`
	Status   string `json:"status"`   // pending | in_progress | completed
	Priority string `json:"priority"` // high | medium | low（空 = medium）
}

// Store 清单存取（会话域实现由装配层注入：写 = 落会话事件流扇出）。
type Store interface {
	Set(items []Item)
}

// Config 构造配置。
type Config struct {
	Store Store
}

type writeIn struct {
	Todos []Item `json:"todos"`
}

// NewTools 构造 todo_write（会话域状态，不经审批——非业务数据）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	t, err := tools.InferTool("todo_write",
		"维护本轮任务的待办清单（全量覆盖写，每次提交完整清单）。多步任务（如批量创建需求、采集多个仓库、撰写周报）开始时先列清单，每完成一步更新对应条目状态——界面会实时向用户展示进度。status 取值 pending|in_progress|completed，同一时刻至多一条 in_progress；priority 取值 high|medium|low。",
		func(_ context.Context, in writeIn) (map[string]any, error) {
			return run(cfg, in)
		})
	if err != nil {
		return nil, err
	}
	return []contract.Tool{t}, nil
}

func run(cfg Config, in writeIn) (map[string]any, error) {
	if len(in.Todos) == 0 {
		return fail("清单不能为空——任务完成后清单应保留全部 completed 条目，而非清空")
	}
	if len(in.Todos) > 50 {
		return fail("清单过长（≤50 条）——拆分为多轮任务")
	}
	inProg := 0
	for i := range in.Todos {
		it := &in.Todos[i]
		if it.Content == "" {
			return fail("第 " + strconv.Itoa(i+1) + " 条 content 为空")
		}
		if len([]rune(it.Content)) > 200 {
			return fail("第 " + strconv.Itoa(i+1) + " 条 content 过长（≤200 字）——条目是一句话，不是段落")
		}
		switch it.Status {
		case "pending", "completed":
		case "in_progress":
			inProg++
			if inProg > 1 {
				return fail("至多一条 in_progress——串行推进，完成一条再开一条")
			}
		default:
			return fail("第 " + strconv.Itoa(i+1) + " 条 status 非法：" + it.Status)
		}
		if it.Priority == "" {
			it.Priority = "medium"
		}
		switch it.Priority {
		case "high", "medium", "low":
		default:
			return fail("第 " + strconv.Itoa(i+1) + " 条 priority 非法：" + it.Priority)
		}
	}
	if cfg.Store != nil {
		cfg.Store.Set(in.Todos)
	}
	done := 0
	for _, it := range in.Todos {
		if it.Status == "completed" {
			done++
		}
	}
	return map[string]any{"ok": true, "todos": in.Todos, "count": len(in.Todos), "completed": done}, nil
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠（errFeed 语义）
}
