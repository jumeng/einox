// spawn 后台派生（Phase W，B 方案 = findings/2026-08-28-background-spawn-plan.md）：
// spawn{background:true} 调用即回 agentId，父回合继续；子代理自建 Runner 泵
// 跑完（事件走 Record 扇出，与主 SSE 三路同链路），结论经通知注入回传父模型
// （running=排队 / idle=自续轮，session.ContinueOrNotify 单锁原子裁定）。
//
// 生命周期红线（对照审查 W-0b 整改）：
//   - 起跑预检：父 ctx 已取消 → 拒绝派生（dsh pre-aborted refusal 同款）
//   - panic 容错：后台 goroutine defer recover → failed 收尾，绝不冒泡杀进程
//   - 取消≠释放：停止/删除全停（CancelSpawns），额度收尾完成才释放（dsh stopping 语义）
//   - 自激护栏：连续自续预算 3（用户消息恢复；通知自身不恢复）；预算耗尽
//     只入队不自续（下轮用户交互消费）；滞留尾巴自续仅对 ended 自然终态——
//     error（用户停止）只入队不自续（防把中断洗成模型请求）
//   - spawn_id 会话域单调分配（跨回合后台任务并存，回合内自增会撞键）
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// maxNotifyWakes 连续自续上限（自激护栏：后台完成→自续→再派后台→再完成
// 的雪崩链有界；用户真实消息恢复预算）。
const maxNotifyWakes = 3

// noopEmit 后台执行体的事件回调（事件已由 Record 落流扇出——泵的 fn 通道
// 仅父流在用，后台无父流）。
func noopEmit(session.Event) {}

// bgEntry 注册表条目（后台任务在途句柄）。
type bgEntry struct {
	cancel context.CancelFunc
}

// bgRegistry 会话域 spawn 注册表 + 并发信号量（同步/后台同一池；后台占用
// 子闸 bgGate 保同步保留额 min(2,cap)）。sem/bgGate nil = 不限（装配
// MaxConcurrent=0 时——限流失效是装配责任，与现状语义一致）。
type bgRegistry struct {
	next    int
	sem     chan struct{}
	bgGate  chan struct{}
	mu      sync.Mutex
	entries map[string]*bgEntry
}

// alloc 分配会话域唯一 spawn_id（sp1/sp2/…）。
func (r *bgRegistry) alloc() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return fmt.Sprintf("sp%d", r.next)
}

// add / remove 注册表登记与注销（生命周期：注册表随会话常驻——同步 spawn
// 不进 entries 但共享 sem，若按后台清空即回收会令下次 assemble 新建满容量
// 信号量、绕过并发上限〔自审 A2〕；回收仅经 DropSpawnReg 挂会话删除端点）。
func (r *bgRegistry) add(id string, cancel context.CancelFunc) {
	r.mu.Lock()
	if r.entries == nil {
		r.entries = map[string]*bgEntry{}
	}
	r.entries[id] = &bgEntry{cancel: cancel}
	r.mu.Unlock()
}

func (r *bgRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.entries, id)
	r.mu.Unlock()
}

// cancelAll 全部取消（停止按钮=全停 / 会话删除；幂等）。
func (r *bgRegistry) cancelAll() {
	r.mu.Lock()
	es := make([]*bgEntry, 0, len(r.entries))
	for _, e := range r.entries {
		es = append(es, e)
	}
	r.mu.Unlock()
	for _, e := range es {
		e.cancel()
	}
}

// spawnReg 会话域注册表懒建（cap<=0 = 不限并发：sem/bgGate 均不设——后台
// 同步一并不限，装配纪律）。cap<=2 时保留额=cap，后台额度 0（bgGate 容量
// 0）——spawn 后台调用被拒并引导同步。
func (m *Manager) spawnReg(s *session.Session, cap int) *bgRegistry {
	m.bgMu.Lock()
	defer m.bgMu.Unlock()
	if m.bg == nil {
		m.bg = map[string]*bgRegistry{}
	}
	if r, ok := m.bg[s.SID]; ok {
		return r
	}
	r := &bgRegistry{}
	if cap > 0 {
		reserve := 2
		if cap < reserve {
			reserve = cap
		}
		r.sem = make(chan struct{}, cap)
		r.bgGate = make(chan struct{}, cap-reserve)
	}
	m.bg[s.SID] = r
	return r
}

// CancelSpawns 会话域后台任务全停（api 停止端点与删除端点挂接；空注册表
// no-op）。停止=全停是 2026-08-28 裁定推荐项。
func (m *Manager) CancelSpawns(sid string) {
	m.bgMu.Lock()
	r, ok := m.bg[sid]
	m.bgMu.Unlock()
	if ok {
		r.cancelAll()
	}
}

// DropSpawnReg 会话删除时回收注册表（自审 A2：常驻生命周期唯一下降沿——
// 取消先行（CancelSpawns），在途 goroutine 仅持 reg 对象引用，map 删除后
// 其 remove/释放操作照常无害）。
func (m *Manager) DropSpawnReg(sid string) {
	m.bgMu.Lock()
	delete(m.bg, sid)
	m.bgMu.Unlock()
}

// bgSpawnTool spawn 双语义外层（W-2）：background 参数分叉——true 走后台
// 派生（即回 agentId），false/缺省 转发同步执行体（H2 既有链路零变化）。
type bgSpawnTool struct {
	m        *Manager
	s        *session.Session
	cfg      *SubAgentsConfig
	inner    tool.InvokableTool // 同步路径（throttledSpawn → spawnFailFeed → agent_tool）
	buildSub func(ctx context.Context, mode string) (*adk.ChatModelAgent, error)
}

func (b *bgSpawnTool) Info(ctx context.Context) (*schema.ToolInfo, error) { return b.inner.Info(ctx) }

func (b *bgSpawnTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	var in struct {
		Background bool `json:"background"`
	}
	if json.Unmarshal([]byte(args), &in) == nil && in.Background {
		return b.start(ctx, args)
	}
	return b.inner.InvokableRun(ctx, args, opts...)
}

// start 后台派生入口：预检 → 注册 → go 执行体（额度获取在 goroutine 内）
// → 即回 agentId。失败一律 errFeed JSON 回喂（与同步路径 spawnFailFeed 同
// 语义——工具 error 会终止父回合，预检失败是子任务失败不是父运行失败）。
// **额度获取不在工具调用内排队**（服务器首跑实证修正）：后台派生的语义是
// 即回——额度满时阻塞工具调用=把后台等待转嫁父回合（可达小时级）且同轮
// 双后台直接环死锁；排队移入 goroutine，父回合零阻塞。
func (b *bgSpawnTool) start(ctx context.Context, args string) (string, error) {
	fail := func(format string, a ...any) (string, error) {
		out, _ := json.Marshal(map[string]any{"ok": false, "error": fmt.Sprintf(format, a...)})
		return string(out), nil
	}
	// 起跑预检（W-0b B-1）：父执行体已取消 → 拒绝，不返回 no-op id
	if err := ctx.Err(); err != nil {
		return fail("父任务已取消，拒绝派生后台子代理：%v", err)
	}
	reg := b.m.spawnReg(b.s, b.cfg.MaxConcurrent)
	if reg.bgGate != nil && cap(reg.bgGate) == 0 {
		return fail("并发上限（%d）不足以预留同步额度，后台派生不可用——请改用同步 spawn", b.cfg.MaxConcurrent)
	}
	// 注册 + 脱离父回合的执行 ctx（后台生命周期归会话域：父轮结束不杀；取消
	// 仅经 CancelSpawns / 进程退出）。operator 等契约值继承会话事实。
	id := reg.alloc()
	bgCtx, cancel := context.WithCancel(context.Background())
	bgCtx = contract.WithOperator(bgCtx, b.s.Owner)
	bgCtx = contract.WithChangeRecorder(bgCtx, b.s.RecordFileChange)
	bgCtx = contract.WithImageInput(bgCtx, b.m.imageCapableOf(b.s))
	reg.add(id, cancel)

	task := spawnTaskOf(args)
	go b.m.runSpawnBG(bgCtx, b.s, reg, id, task, args, b.buildSub)

	out, _ := json.Marshal(map[string]any{"agentId": id, "spawn_id": id, "status": "background"})
	return string(out), nil
}

// spawnTaskOf 参数里取 task 摘要（通知文本用；解析失败回退整段截断）。
func spawnTaskOf(args string) string {
	var in struct {
		Task string `json:"task"`
	}
	if json.Unmarshal([]byte(args), &in) == nil && in.Task != "" {
		return in.Task
	}
	return truncateRunes(args, 80)
}

// runSpawnBG 后台执行体：bg 档子代理自建 Runner + 泵翻译（EvSubAgent 带
// spawn_id，走 Record 扇出）→ done/failed 终态事件 + 通知注入。
// 过程事件（text/call/result）随 EmitEvents 装配档（与同步路径同语义——
// 关档静默）；done/failed 终态**必发**（前端封口与通知卡数据源，功能面非
// 展示档）。结论 = 最后一段 assistant 文本（工具循环的中间段不拼入——
// agent_tool lastEvent 同语义）。
// defer 链（对照审查 A-1/A-2）：recover → failed 收尾；注销注册表；释放额度
// （取消≠释放，收尾完成才释放——dsh stopping 语义）。
// acquireBG 额度获取（先后台子闸再主池——顺序统一；同步路径只占主池，
// 保留额由 bgGate 保证）。在后台 goroutine 内排队（start 即回语义——服务器
// 首跑实证：工具调用内排队会阻塞父回合且同轮双后台环死锁）；取消 = 排队的
// 合法出口（failed 终态封口、不通知）。false = 已收尾（取消），调用方即返。
func (m *Manager) acquireBG(ctx context.Context, s *session.Session, reg *bgRegistry, id, task string) bool {
	if reg.bgGate != nil {
		select {
		case reg.bgGate <- struct{}{}:
		case <-ctx.Done():
			m.finishSpawnBG(s, id, task, "", errors.New("后台子代理已停止（等待并发额度时取消）"), false)
			return false
		}
	}
	if reg.sem != nil {
		select {
		case reg.sem <- struct{}{}:
		case <-ctx.Done():
			if reg.bgGate != nil {
				<-reg.bgGate
			}
			m.finishSpawnBG(s, id, task, "", errors.New("后台子代理已停止（等待并发额度时取消）"), false)
			return false
		}
	}
	return true
}

func (m *Manager) runSpawnBG(ctx context.Context, s *session.Session, reg *bgRegistry, id, task, args string, buildSub func(context.Context, string) (*adk.ChatModelAgent, error)) {
	if !m.acquireBG(ctx, s, reg, id, task) {
		return
	}
	// defer 链（对照审查 A-1/A-2）：recover → failed 收尾；注销注册表；释放
	// 额度（取消≠释放，收尾完成才释放——dsh stopping 语义）。
	defer func() {
		if r := recover(); r != nil {
			m.finishSpawnBG(s, id, task, "", fmt.Errorf("后台子代理 panic：%v", r), true)
		}
		reg.remove(id)
		if reg.sem != nil {
			<-reg.sem
		}
		if reg.bgGate != nil {
			<-reg.bgGate
		}
	}()

	sub, err := buildSub(ctx, "bg")
	if err != nil {
		m.finishSpawnBG(s, id, task, "", fmt.Errorf("后台子代理构造失败：%w", err), true)
		return
	}
	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: sub, EnableStreaming: true})
	iter := runner.Run(ctx, []*schema.Message{schema.UserMessage(args)})

	// 泵：迭代子代理事件 → EvSubAgent 翻译（spawn_id 归组键）+ 末段结论累积。
	// 同步路径同翻译器（emitSubAgent）；子事件只进事件流不进父历史/上下文。
	calls := map[string]string{}
	lastText := ""
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			if s.Stopped() {
				return // 删除：静默收线（无终态事件——会话域已终）
			}
			if errors.Is(ev.Err, context.Canceled) {
				// 停止=全停的取消（会话活着）：发 failed 终态封口前端卡与
				// watch 保活，但**不通知注入**——用户刚按停止，再唤醒=把中断
				// 洗成模型请求（自审 A3）
				m.finishSpawnBG(s, id, task, "", errors.New("后台子代理已停止（会话停止/删除）"), false)
				return
			}
			m.finishSpawnBG(s, id, task, "", fmt.Errorf("后台子代理执行失败：%s", truncateRunes(llm.Classify(unwrapRetryExhausted(ev.Err)).Message, 200)), true)
			return
		}
		if ev.Action != nil && ev.Action.Interrupted != nil {
			// 防御路径（正常白名单下无审批/提问面）：后台无人决议——failed 收尾
			m.finishSpawnBG(s, id, task, "", errors.New("子代理请求人工交互（审批/提问），后台不可用——请调整白名单或改用同步 spawn"), true)
			return
		}
		if ev.Output != nil && ev.Output.MessageOutput != nil {
			// 流的唯一消费者：开档 emitSubAgent（顺带记末段结论）；关档
			// recordLastText 自行排空——MessageStream 不可双读
			if m.subEventsOn() {
				m.emitSubAgent(s, noopEmit, spawnToolName, calls, ev.Output.MessageOutput, id, &lastText)
			} else {
				recordLastText(ev.Output.MessageOutput, &lastText)
			}
		}
	}
	if s.Stopped() {
		return
	}
	m.finishSpawnBG(s, id, task, lastText, nil, true)
}

// recordLastText 末段 assistant 文本累积（覆盖式——多轮工具循环的中间段
// 不进结论，最终保留最后一段非空文本；与 agent_tool lastEvent 同语义）。
// emitSubAgent 的流式排空在此前完成与否不影响——本函数独立读流前先判
// IsStreaming 形态：流式由 emitSubAgent 排空（subEventsOn 档）或由本函数
// 排空（关档），二者互斥。
func recordLastText(v *adk.TypedMessageVariant[*schema.Message], last *string) {
	if v.Role != schema.Assistant {
		return
	}
	if v.IsStreaming && v.MessageStream != nil {
		var b strings.Builder
		willRetry := false
		for {
			chunk, err := v.MessageStream.Recv()
			if err != nil {
				var wr *adk.WillRetryError
				willRetry = errors.As(err, &wr) // 网络容错 ②：半截不作结论（覆盖式语义自愈，防御性跳过）
				break
			}
			if chunk != nil {
				b.WriteString(chunk.Content)
			}
		}
		if !willRetry && b.Len() > 0 {
			*last = b.String()
		}
		return
	}
	if v.Message != nil && v.Message.Content != "" {
		*last = v.Message.Content
	}
}

// bgConclusionLimit 后台结论事件/通知的入流上限（超长外置 spill 域——
// read_file 虚拟前缀取回，H1 既有管线；写失败保底纯截断）。
const bgConclusionLimit = 4000

// offloadConclusion 超长结论外置（W-2 方案 §2：sessions/<sid>/spill/spawn/
// <spawn_id> 经 Store 写；模型面虚拟路径 spill/spawn/<spawn_id> 与 fsutil
// 取回前缀一致——父/子 read_file 均路由）。返回入流文本（截断+取回指引）。
func (m *Manager) offloadConclusion(s *session.Session, id, text string) string {
	r := []rune(text)
	if len(r) <= bgConclusionLimit {
		return text
	}
	rel := path.Join("sessions", s.SID, "spill", "spawn", id)
	if err := m.reg.Store().WriteUserTreeFile(s.Owner, rel, []byte(text)); err != nil {
		// 外置是增强非关键面：写失败保底纯截断（指引路径无文件会误导，不带）
		return string(r[:bgConclusionLimit]) + "…（结论超长已截断——完整结论见会话事件流该子代理卡）"
	}
	return string(r[:bgConclusionLimit]) + fmt.Sprintf("…\n（结论超长已外置——全文经 read_file spill/spawn/%s 取回）", id)
}

// finishSpawnBG 终态收尾：done/failed 事件（结论/errFeed 信封）+ 通知注入
// （notify=true 时；自激护栏内）。事件先行、通知最后（dsh
// completion-announced-last 同款：通知可能同步开自续轮，其余观察者须已见
// 终态）。notify=false 用于用户主动停止的取消（终态封口但不打扰）。
func (m *Manager) finishSpawnBG(s *session.Session, id, task, conclusion string, failure error, notify bool) {
	if s.Stopped() {
		return // 已删除：磁盘零残留
	}
	text := conclusion
	if failure != nil {
		b, _ := json.Marshal(map[string]any{"ok": false, "error": failure.Error()})
		text = string(b)
	}
	kind := "done"
	if failure != nil {
		kind = "failed"
	}
	flow := m.offloadConclusion(s, id, text) // 一次外置，事件与通知共用（指引一致）
	m.emit(s, noopEmit, contract.EvSubAgent, contract.SubAgentEvent{
		SpawnID: id, Agent: spawnToolName, Kind: kind,
		Text: flow,
	})

	if !notify || s.Stopped() { // 终态事件后删除竞态：注入无意义
		return
	}
	m.NotifyOwner(s, notifyText(id, task, flow, failure == nil))
}

// notifyText 通知文本（提示词配套教学格式）：完成=结论（已外置/截断形态，
// 与事件 Text 同源——超长时带 read_file 取回指引）；失败=errFeed 信封原文。
func notifyText(id, task, payload string, ok bool) string {
	head := "[后台子代理完成] "
	if !ok {
		head = "[后台子代理失败] "
	}
	return head + truncateRunes(task, 120) + "\n结论：\n" + payload
}

// NotifyOwner 完成通知注入（W-3 核心）：自激护栏（预算耗尽只入队不自续）→
// 单锁原子裁定（running=排队 / idle=自续轮）→ 滞留尾巴兜底（仅 ended）。
func (m *Manager) NotifyOwner(s *session.Session, note string) {
	if s.Stopped() {
		return
	}
	allowWake := s.NotifyBudget(maxNotifyWakes) // 消费一格；false=耗尽（下轮用户交互恢复）
	began, q := s.ContinueOrNotify(note, allowWake)
	s.Record(contract.EvNotifyQueued, contract.SteerEvent{ID: q.ID, Text: q.Text, Kind: "notify"})
	if began {
		go m.Run(context.Background(), s, "", nil, noopEmit) // 自续轮：通知经队列注入（输入路径唯一）
		return
	}
	// 滞留尾巴兜底：入队时父轮在跑（或挂起）——挂 runDone 等收尾；自然结束
	// （ended）且队列仍滞留则自续消费；error（用户停止）/pending（决议续流）
	// /新轮已起（running）都不动——把用户中断洗成模型请求是被明确拒绝的形态。
	go func() {
		done := s.RunDone()
		if done == nil {
			return
		}
		<-done
		if s.StateOf() != session.StateEnded || s.QueueLen() == 0 {
			return
		}
		if s.BeginRun("") {
			go m.Run(context.Background(), s, "", nil, noopEmit)
		}
	}()
}
