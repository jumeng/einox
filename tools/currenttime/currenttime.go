// Package currenttime 提供 get_current_time 工具：时钟/周界语义——周报周期
// 与 deadline 计算的正确性底线。行为参照 openai/codex
// core/src/current_time.rs（Apache-2.0，语义移植非文本移植）。
package currenttime

import (
	"context"
	"fmt"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// NewTools 构造 get_current_time（进程级无状态，直读）。
func NewTools() ([]contract.Tool, error) {
	t, err := tools.InferTool("get_current_time",
		"获取当前日期时间：日期、星期、ISO 周号、本周一/周日边界、时区。凡涉及「今天/本周/下周五/截止日/第几周」等时间计算，必须先调用本工具取得事实，不要凭训练记忆猜日期。",
		run)
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(t, contract.BehaviorRead)}, nil
}

func run(_ context.Context, _ struct{}) (map[string]any, error) {
	now := time.Now()
	weekday := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(now.Weekday())]
	_, w := now.ISOWeek()
	// ISO 周以周一为首：偏移换算（周一=0）
	off := (int(now.Weekday()) + 6) % 7
	monday := now.AddDate(0, 0, -off)
	return map[string]any{
		"now":        now.Format("2006-01-02 15:04:05"),
		"date":       now.Format("2006-01-02"),
		"weekday":    weekday,
		"iso_week":   fmt.Sprintf("W%02d", w),
		"week_start": monday.Format("2006-01-02"),
		"week_end":   monday.AddDate(0, 0, 6).Format("2006-01-02"),
		"timezone":   now.Format("-07:00"),
	}, nil
}
