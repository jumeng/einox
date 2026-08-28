package session

// Detail/DetailDisk since 增量过滤回归（2026-08-26 会话切换提速）：since>0 只回
// id>since 尾段；since=0 全量；越过末位 = 空。内存与盘上两路同测。

import (
	"path/filepath"
	"testing"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

func TestDetailSince(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)

	// 内存会话：Record 三事件（id 自增 1..3）
	s := reg.Create("张三", "任务", "plan", contract.UserPrefs{})
	e1 := s.Record(contract.EvUserMessage, contract.UserMsg{Text: "一"})
	s.Record(contract.EvTextDelta, contract.Delta{Delta: "二"})
	e3 := s.Record(contract.EvTextDelta, contract.Delta{Delta: "三"})

	if d, ok := reg.Detail(s.SID, 0); !ok || len(d.Events) != 3 {
		t.Fatalf("since=0 应回全量 3 条, got %d ok=%v", len(d.Events), ok)
	}
	if d, ok := reg.Detail(s.SID, e1.ID); !ok || len(d.Events) != 2 || d.Events[0].ID != e1.ID+1 {
		t.Fatalf("since=%d 应回尾段 2 条, got %+v", e1.ID, d.Events)
	}
	if d, ok := reg.Detail(s.SID, e3.ID); !ok || len(d.Events) != 0 {
		t.Fatalf("since=末位 应回空, got %d", len(d.Events))
	}
}

func TestDetailDiskSince(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	doc := `{"sid":"d9","owner":"张三","task":"t","title":"T","state":"ended",` +
		`"started_at":"2026-08-26T01:00:00Z","updated_at":"2026-08-26T02:00:00Z",` +
		`"events":[{"id":1,"event":"user_message","data":{"text":"a"}},` +
		`{"id":2,"event":"text_delta","data":{"delta":"b"}},` +
		`{"id":3,"event":"text_delta","data":{"delta":"c"}}]}`
	if err := st.WriteUserTreeFile("张三", filepath.Join("sessions", "d9", "session.json"), []byte(doc)); err != nil {
		t.Fatal(err)
	}

	if d, ok := reg.DetailDisk("张三", "d9", 1); !ok || len(d.Events) != 2 || d.Events[0].ID != 2 {
		t.Fatalf("since=1 应回 id 2,3, got %+v", d.Events)
	}
	if d, ok := reg.DetailDisk("张三", "d9", 0); !ok || len(d.Events) != 3 {
		t.Fatalf("since=0 应回全量, got %d", len(d.Events))
	}
	if d, ok := reg.DetailDisk("张三", "d9", 3); !ok || len(d.Events) != 0 {
		t.Fatalf("since=3 应回空, got %d", len(d.Events))
	}
}
