package session

// 并发 Persist 写序回归（消费者驱动：einox-pm 决议/排队编辑端点与引擎泵
// 并发调用 Registry.Persist 同一会话）。修复前 persist 快照持锁但提交无序
// ——旧快照若后提交即覆盖新快照（事件/决议/队列编辑在盘上丢失，重启
// Reattach 才显形）；修复 = 持 s.mu 时获取每会话写序锁，快照序即提交序。
// 本测试以「同路径写永不重叠」钉住机制（重叠 = 写序失效的直接观测）。

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/internal/tstore"
)

// gatingStore 首写门控的 Store 包装：门控期内所有 WriteUserTreeFile 进入即
// 挂起（放行通道关闭后直通）；期间同路径再入即重叠（写序失效的观测点）。
type gatingStore struct {
	Store
	gated   atomic.Bool // 门控期标记
	release chan struct{}
	inWrite atomic.Int32
	overlap atomic.Bool
}

func (g *gatingStore) WriteUserTreeFile(op, rel string, data []byte) error {
	if n := g.inWrite.Add(1); n > 1 {
		g.overlap.Store(true)
	}
	defer g.inWrite.Add(-1)
	if g.gated.Load() {
		<-g.release // 门控期：首写在途（若第二写也到达此处即重叠已计）
	}
	return g.Store.WriteUserTreeFile(op, rel, data)
}

// TestPersistWritesNeverOverlap 首写在途时并发第二落盘：写序锁保证第二写
// 的提交等待首写完成——store 层面同会话写永不重叠（修复前第二次 persist
// 与首写并发进入 store）。
func TestPersistWritesNeverOverlap(t *testing.T) {
	g := &gatingStore{Store: tstore.New(t.TempDir()), release: make(chan struct{})}
	reg := NewRegistry(g)
	s := reg.Create("张三", "任务", "manual", contract.UserPrefs{}) // Create 内部落盘须在门控前置位外
	s.Record("user_message", map[string]string{"text": "第一条"})
	g.gated.Store(true) // 门控从此刻起（只拦测试发起的两次落盘）

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reg.Persist(s) }()
	time.Sleep(50 * time.Millisecond) // 首写进入门控在途
	go func() { defer wg.Done(); reg.Persist(s) }()
	time.Sleep(50 * time.Millisecond) // 第二写应阻塞在写序锁（不进 store）

	close(g.release) // 放行（closed 后接收即返）
	wg.Wait()
	g.gated.Store(false)
	if g.overlap.Load() {
		t.Fatal("同一会话的两次落盘写重叠——快照序未锁定提交序（丢更新窗口开敞）")
	}
}

// TestPersistOrderIsSnapshotOrder 顺序性：并发落盘全部完成后，终盘含全部
// 事件（后快照不因在途旧写覆盖而回退）。
func TestPersistOrderIsSnapshotOrder(t *testing.T) {
	st := tstore.New(t.TempDir())
	reg := NewRegistry(st)
	s := reg.Create("李四", "任务", "auto", contract.UserPrefs{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		s.Record("user_message", map[string]string{"text": fmt.Sprintf("m%d", i)})
		wg.Add(1)
		go func() { defer wg.Done(); reg.Persist(s) }()
	}
	wg.Wait()
	reg2 := NewRegistry(st)
	s2 := reg2.Reattach("李四", s.SID)
	if s2 == nil {
		t.Fatal("Reattach 应续接会话")
	}
	if n := len(s2.SnapshotEvents()); n != 8 {
		t.Fatalf("终盘应含全部 8 条事件（旧写覆盖新写即丢），实得 %d", n)
	}
}
