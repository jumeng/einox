package session

// 会话搜索回归（2026-08-26 会话历史搜索面）：q 过滤 = 标题（Title||Task 回退）
// 或任一 user_message 事件文本包含关键词，不区分大小写；空 q = 同 List。
// 覆盖内存会话与盘上会话两路（listOwner 双循环）+ SearchAll 跨用户。

import (
	"path/filepath"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// mkSearchDisk 盘上会话（title/task + events 数组——search 路径轻解析的输入）。
func mkSearchDisk(t *testing.T, st *tstore.Store, owner, sid, title, task, eventsJSON string) {
	t.Helper()
	doc := `{"sid":"` + sid + `","owner":"` + owner + `","task":"` + task + `","title":"` + title +
		`","state":"ended","started_at":"2026-08-26T01:00:00Z","updated_at":"2026-08-26T02:00:00Z",` +
		`"events":[` + eventsJSON + `]}`
	if err := st.WriteUserTreeFile(owner, filepath.Join("sessions", sid, "session.json"), []byte(doc)); err != nil {
		t.Fatal(err)
	}
}

func sidsOf(items []SessionListItem) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.SID] = true
	}
	return out
}

func TestSearch(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)

	// 盘上三会话：标题命中 / 用户消息命中（大小写不敏感）/ 全不命中
	mkSearchDisk(t, st, "张三", "d111", "周报整理", "整理本周周报", "")
	mkSearchDisk(t, st, "张三", "d222", "", "随便干点",
		`{"id":1,"event":"user_message","data":{"text":"帮我把里程碑 M5 排进计划"}}`)
	mkSearchDisk(t, st, "张三", "d333", "无关标题", "无关任务",
		`{"id":1,"event":"text_delta","data":{"delta":"周报"}}`) // 助手输出不算 prompts
	// 他人会话：mine 搜索不可见
	mkSearchDisk(t, st, "李四", "d444", "李四的周报", "", "")

	// 内存会话：标题 + 用户消息两路命中
	s := reg.Create("张三", "任务梗概", "plan", contract.UserPrefs{})
	s.SetTitle("内存会话标题")
	s.Record(contract.EvUserMessage, contract.UserMsg{Text: "再帮我查一下 Delivery 板块"})

	cases := []struct {
		q    string
		want map[string]bool
	}{
		{"周报", map[string]bool{"d111": true}},         // 标题命中（含 Task 回退）
		{"M5", map[string]bool{"d222": true}},         // 用户消息命中（大小写不敏感）
		{"delivery 板块", map[string]bool{s.SID: true}}, // 内存会话用户消息命中（全角空格原样匹配）
		{"内存", map[string]bool{s.SID: true}},          // 内存会话标题命中
		{"周报 整理", map[string]bool{}},                  // 整串无命中（不做分词）
		{"", nil /* 空 q = 不过滤，走 List 全集 */},
	}
	for _, c := range cases {
		got := sidsOf(reg.Search("张三", c.q))
		if c.q == "" {
			// 空 q 退化为 List：全集 = 盘上 3 + 内存 1（李四不可见）
			if len(got) != 4 || got["d444"] {
				t.Fatalf("Search(空) 应回退 List 全集, got %v", got)
			}
			continue
		}
		if len(got) != len(c.want) {
			t.Fatalf("Search(%q) = %v, want %v", c.q, got, c.want)
		}
		for sid := range c.want {
			if !got[sid] {
				t.Fatalf("Search(%q) 缺 %s, got %v", c.q, sid, got)
			}
		}
	}

	// SearchAll 跨用户（admin 视图）：李四的会话按其标题命中
	all := sidsOf(reg.SearchAll("李四"))
	if len(all) != 1 || !all["d444"] {
		t.Fatalf("SearchAll(李四) = %v, want [d444]", all)
	}
}
