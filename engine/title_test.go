package engine

// 标题行为测：DayHeader 表测（提示词日期头）与首轮标题生成（含挂起+Resume
// 场景的 firstTurn 判定——U-1，2026-09-07）。

import (
	"testing"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// TestSuspendedFirstTurnGeneratesTitle U-1：经审批挂起 + Resume 收尾的首轮
// 应生成标题。此前 settleTurn 在收尾时以「历史已有 assistant」判非首轮——
// 挂起段已 flushAcc 入史，同一首轮被污染，标题恒回退 Task（实测踩坑记录
// findings/2026-09-07-assembly-skill-fieldtest.md）。修后 firstTurn 锚定
// Run 入口（挂起发生前），跨挂保持到 Resume 收尾。
func TestSuspendedFirstTurnGeneratesTitle(t *testing.T) {
	m, s, _, _, _ := mergedSetup(t)
	if err := m.Channels().Approve(s.SID, "", nil, contract.ApprovalDecision{Approve: true}); err != nil {
		t.Fatalf("批量决议应成功：%v", err)
	}
	waitFor(t, "批准应正常收口", func() bool { return s.StateOf() == session.StateEnded })
	waitTitleFlight(t, s)
	// scriptedModel.Generate 固定回「非流式答复」——genTitle 走 Generate，
	// 修复生效即以此为标题；未生成时 TitleOf 为空（Task 回退是消费侧行为）。
	if got := s.TitleOf(); got != "非流式答复" {
		t.Fatalf("挂起+Resume 收尾的首轮应生成标题，实得 %q", got)
	}
}

func TestDayHeader(t *testing.T) {
	cases := []struct {
		date string
		want string
	}{
		// 年边界：2026-01-01 周四，W01 的周界跨年（周一落在 2025-12-29）
		{"2026-01-01", "今天是 2026-01-01 周四（W01，本周 2025-12-29 至 2026-01-04）。"},
		// 周日归上一周：同属 W01
		{"2026-01-04", "今天是 2026-01-04 周日（W01，本周 2025-12-29 至 2026-01-04）。"},
		// 周一开新周：W02
		{"2026-01-05", "今天是 2026-01-05 周一（W02，本周 2026-01-05 至 2026-01-11）。"},
		// 闰年二月末：2024-02-29 周四，W09
		{"2024-02-29", "今天是 2024-02-29 周四（W09，本周 2024-02-26 至 2024-03-03）。"},
		// 年边界（53 周年）：2027-01-01 周五归 2026 的 W53
		{"2027-01-01", "今天是 2027-01-01 周五（W53，本周 2026-12-28 至 2027-01-03）。"},
	}
	for _, c := range cases {
		d, err := time.Parse("2006-01-02", c.date)
		if err != nil {
			t.Fatal(err)
		}
		if got := DayHeader(d); got != c.want {
			t.Fatalf("DayHeader(%s) 实得 %q（期望 %q）", c.date, got, c.want)
		}
	}
}
