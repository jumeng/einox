// Package calendar ISO 周界单点（engine.DayHeader 与 currenttime 工具共用
// ——审查 P3-4：周界数学曾两处各写，时区/年边修正需双改）。展示文案
// （中文星期数组、W%02d 格式）属各消费面的呈现选择，留本地。
package calendar

import "time"

// Monday 本周周一（ISO 周以周一为首——周日归上一周）。
func Monday(now time.Time) time.Time {
	return now.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
}
