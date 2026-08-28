package currenttime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCurrentTime(t *testing.T) {
	ts, err := NewTools()
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	info := ts[0].Info()
	if info.Name != "get_current_time" {
		t.Fatalf("工具名：%s", info.Name)
	}
	out, err := ts[0].Invoke(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	t.Logf("输出：%s", out)

	// 周界自检：week_start 必为周一、week_end 为周日、含今天
	now := time.Now()
	off := (int(now.Weekday()) + 6) % 7
	for _, want := range []string{
		now.AddDate(0, 0, -off).Format("2006-01-02"),  // 本周一
		now.AddDate(0, 0, 6-off).Format("2006-01-02"), // 本周日
		now.Format("2006-01-02"),                      // 今天
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("输出缺 %s：%s", want, out)
		}
	}
}
