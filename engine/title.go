package engine

// 标题与杂项：首轮判定（hasAssistant）、异步标题生成（genTitle ≤16 字中文）
// 与清洗（sanitizeTitle）、提示词通用日期头（DayHeader）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/internal/calendar"
	"github.com/jumeng/einox/internal/strutil"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// hasAssistant 历史中是否已有 assistant 终态（首轮判定——用户消息自 Run
// 开头即入史，不能再以「历史为空」判首轮）。
func hasAssistant(msgs []*schema.Message) bool {
	for _, m := range msgs {
		if m.Role == schema.Assistant {
			return true
		}
	}
	return false
}

// genTitle 首轮收尾后异步生成会话标题（≤16 字中文，直接输出）：会话模型快照 +
// effort low（标题是短生成，固定低档思考）+ 15s 超时；失败/超时/已删除断路，
// 列表 title = Title || Task 回退。
func (m *Manager) genTitle(s *session.Session, userMsg, assistant string) {
	if strings.TrimSpace(userMsg) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	providers := m.Opt.Providers()
	p, spec, ok := llm.FindSpec(providers, s.ModelSnapshot().Model) // 持锁快照（PUT settings 随时写并发）
	if !ok {
		return
	}
	cm, err := m.Opt.NewModel(ctx, p, spec, "low")
	if err != nil {
		return
	}
	speaker := "用户" // T6 说话人措辞（单用户零变化）
	if a := s.TurnActorOf(); a != nil && a.Name != "" {
		speaker = a.Name
	}
	prompt := "为下面的任务对话生成一个不超过16个字的中文标题，直接输出标题本身，不要引号、句号或任何解释。\n\n" +
		speaker + "：" + strutil.Truncate(userMsg, 2000) + "\n\n助手：" + strutil.Truncate(assistant, 500)
	out, err := cm.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil || s.Stopped() {
		return
	}
	title := sanitizeTitle(out.Content)
	if title == "" {
		return
	}
	s.SetTitle(title)
	m.reg.Persist(s)
}

// sanitizeTitle 标题清洗：单行、去引号包裹与句读、截 16 字。
func sanitizeTitle(in string) string {
	in = strings.ReplaceAll(in, "\n", " ")
	for _, q := range []string{"\"", "'", "“", "”", "‘", "’", "「", "」", "『", "』", "《", "》"} {
		in = strings.ReplaceAll(in, q, "")
	}
	in = strings.TrimSpace(in)
	in = strings.Trim(in, "。．.…！!？?；;，, ")
	return strutil.Truncate(in, 16)
}

// DayHeader 通用日期头（提示词机制件——业务段拼装归应用，自产品
// instruction.go 的日期头拆出）。
func DayHeader(now time.Time) string {
	monday := calendar.Monday(now) // ISO 周界单点（与 currenttime 工具同源，审查 P3-4）
	_, w := now.ISOWeek()
	return fmt.Sprintf("今天是 %s %s（%s，本周 %s 至 %s）。",
		now.Format("2006-01-02"),
		[...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[int(now.Weekday())],
		fmt.Sprintf("W%02d", w),
		monday.Format("2006-01-02"),
		monday.AddDate(0, 0, 6).Format("2006-01-02"))
}
