// Package mid 是工具中间件链（guard / errFeed）：业务错误回喂模型自纠、
// 防死循环提醒、单工具执行硬上限。全部在契约层包装（自产品 internal/agent
// interrupt.go 的通用件迁入，语义不变——参照 dsh packages/guard[MIT]）。
package mid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jumeng/einox/contract"
)

// ErrFeedTool 工具错误转结果包装：参数校验、业务规则等可恢复错误以
// {"ok":false,"error":…} 工具结果回喂模型自纠——Go error 会被引擎包装成
// NodeRunError 终止整轮，模型不可见即不可调整。生命周期错误（取消/超时）
// 与挂起哨兵（*contract.Suspend）原样上抛——后者转信封会吞掉挂起语义。
func ErrFeed(t contract.Tool) contract.Tool { return &errFeedTool{t: t} }

type errFeedTool struct{ t contract.Tool }

func (w *errFeedTool) Info() *contract.ToolInfo { return w.t.Info() }

func (w *errFeedTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	out, err := w.t.Invoke(ctx, args)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err // 停止/断连生命周期错误照旧上抛
	}
	var su *contract.Suspend
	if errors.As(err, &su) {
		return nil, err // 挂起信号直通
	}
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return b, nil
}

// ToolDeadline 单工具执行硬上限（guard 截止；超时以结果信封回喂。测试可缩）。
var ToolDeadline = 10 * time.Minute

// Guard 防死循环包装：同参连续第 3 次起在结果前注入提醒；执行超时强制截止。
// 计数器随组装实例存续（轮内有效——死循环场景即轮内）；有状态故持锁——
// eino ToolsNode 并发执行同批多个 tool_call 时同一实例被并发 Invoke。
func Guard(t contract.Tool) contract.Tool { return &guardTool{t: t} }

type guardTool struct {
	t        contract.Tool
	lastArgs string
	n        int
	mu       sync.Mutex
}

func (g *guardTool) Info() *contract.ToolInfo { return g.t.Info() }

func (g *guardTool) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	g.mu.Lock()
	if string(args) == g.lastArgs {
		g.n++
	} else {
		g.lastArgs, g.n = string(args), 1
	}
	n := g.n
	g.mu.Unlock()
	gctx, cancel := context.WithTimeout(ctx, ToolDeadline)
	defer cancel()
	out, err := g.t.Invoke(gctx, args)
	if err != nil {
		if errors.Is(gctx.Err(), context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return json.RawMessage(fmt.Sprintf(`{"ok":false,"error":"工具执行超过 %s 硬上限，已截止——拆小任务或改用后台任务（run_command background）"}`, ToolDeadline)), nil
		}
		return nil, err
	}
	if n >= 3 {
		out = json.RawMessage(fmt.Sprintf("⚠ 本工具已连续第 %d 次以相同参数调用——若前次结果已够用勿再重复；若一直失败请换思路（改参数/换工具/ask_user 问用户）。\n%s", n, out))
	}
	return out, nil
}
