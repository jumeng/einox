// channel 渠道编排机制核（传输无关）：外部渠道（即时通信机器人 / 实时语音 /
// 任意消息入口）与引擎之间的会话编排——入站消息分流（空闲起轮 / 运行中排队 /
// 挂起接决议续流）、常驻事件订阅出站（覆盖挂起期与后台通知——live 回调
// fn 生命周期绑定 Run/Resume 调用的既有缺口由订阅面补齐）、停止与主动推送。
// 渠道协议与消息渲染归适配器：应用实现 ChannelSink、调 Handle/Approve/Answer
// 即成一个渠道（官方通用件见 channels/ 子包，业务自定义渠道长在业务仓——
// 同 llm 供应商「内置目录 + 自定义」两层模式）。三层结构与接入蓝图见 docs/04。
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/session"
)

// ChannelSink 渠道出站投递（应用实现：事件 → 渠道消息渲染发送）。契约：
// 单会话串行投递、跨会话并发——实现须并发安全；慢消费由实现侧节流（文本
// 增量可聚合），偶发丢事件由订阅面的水位补投兜底（本面是尽力而为的实时
// 视图，事件流真源在会话记录——回放/对账走 Detail/快照）。
type ChannelSink interface {
	Deliver(b ChannelBrief, ev session.Event)
}

// ChannelBrief 渠道会话上下文（投递时回查：渠道实例/渠道会话键/归属/会话 ID）。
type ChannelBrief struct {
	Channel string
	Chat    string
	Owner   string
	SID     string
}

// InboundMsg 渠道入站消息（适配器解析协议后转投——引擎只见文本轮次，媒体
// 形态处理（语音转写分段/附件提取）归适配器）。Owner 判定策略归适配器
// （渠道账号 ↔ 用户绑定体系在业务层）；Mode 空 = manual（写工具逐次审批，
// 安全缺省）。
type InboundMsg struct {
	Channel     string
	Chat        string
	Owner       string
	Mode        string
	Text        string
	Attachments []session.Attachment
}

// ChannelConfig 渠道实例装配（Options.Channels 条目）。ID 是消息路由键
// （InboundMsg.Channel 匹配），进程内唯一。Model = 渠道会话缺省模型复合键
// （provider/model，渠道新建会话粘住）——不可用键首轮即 CONFIG 错误面如实
// 暴露，装配侧须给可用键。
type ChannelConfig struct {
	ID    string
	Model string
	Sink  ChannelSink
}

// 绑定持久化：渠道会话键 ↔ 会话 ID 映射表经 Store 用户树落一份 JSON——
// 伪 owner（无会话/工作区内容，用户列表侧容忍），单文件原子读写。绑定
// 自愈：会话被删后取回失败即新建覆盖，无需显式清理。
const (
	bindOwner = ".channel-bindings"
	bindRel   = "bindings.json"
)

// bindRecord 单条绑定落盘记录（键 = channel \x00 chat，见 bindKey）。
type bindRecord struct {
	SID   string `json:"sid"`
	Owner string `json:"owner"`
}

// bindFile 绑定表落盘形态。
type bindFile struct {
	Binds map[string]bindRecord `json:"binds"`
}

func bindKey(channel, chat string) string { return channel + "\x00" + chat }

// ChannelGateway 渠道编排泵（Manager.Channels() 出口，懒建总可用——未装配
// 渠道清单时 Handle 对未注册渠道报错，而非 nil 面拒绝）。多渠道并发：绑定
// 键含渠道 ID 天然隔离；一个 Manager（= 一个 agent 装配）一个 Gateway，
// 跨 agent 分流由应用层多 Gateway 组合，基座不建路由器。
type ChannelGateway struct {
	m *Manager

	mu    sync.Mutex
	binds map[string]*channelBind // 运行态（含消费泵）
	disk  bindFile                // 落盘镜像（懒加载；新建/失绑即回写）

	wg     sync.WaitGroup
	stopCh chan struct{}
	closed bool
}

// channelBind 运行态绑定：会话引用 + 订阅通道 + 投递水位（最新已投事件 ID
// ——慢消费丢事件后按 ID 间隙从事件快照补投）。
type channelBind struct {
	brief  ChannelBrief
	s      *session.Session
	sub    chan session.Event
	lastID int
}

func newChannelGateway(m *Manager) *ChannelGateway {
	return &ChannelGateway{
		m: m, binds: map[string]*channelBind{},
		stopCh: make(chan struct{}),
	}
}

// Channels 渠道编排出口（懒建；进程单例）。
func (m *Manager) Channels() *ChannelGateway {
	m.chanMu.Lock()
	defer m.chanMu.Unlock()
	if m.chanGW == nil {
		m.chanGW = newChannelGateway(m)
	}
	return m.chanGW
}

// cfgOf 渠道配置查找（未装配/未注册即拒——消息路由键找不到消费面）。
func (g *ChannelGateway) cfgOf(channel string) (ChannelConfig, bool) {
	for _, c := range g.m.Opt.Channels {
		if c.ID == channel {
			return c, true
		}
	}
	return ChannelConfig{}, false
}

// loadDiskLocked 绑定表落盘读（一次）。文件缺失 = 首个渠道会话前无绑定，
// 空表；损坏同样空表重建（逐渠道会话自愈，不阻塞接入）。mu 持有者调用。
func (g *ChannelGateway) loadDiskLocked() {
	if g.disk.Binds != nil {
		return
	}
	g.disk.Binds = map[string]bindRecord{}
	if data, ok := g.m.reg.Store().ReadUserTreeFile(bindOwner, bindRel); ok {
		_ = json.Unmarshal(data, &g.disk)
		if g.disk.Binds == nil {
			g.disk.Binds = map[string]bindRecord{}
		}
	}
}

// saveDiskLocked 绑定表回写。mu 持有者调用。
func (g *ChannelGateway) saveDiskLocked() {
	data, err := json.Marshal(g.disk)
	if err != nil {
		return
	}
	_ = g.m.reg.Store().WriteUserTreeFile(bindOwner, bindRel, data)
}

// establishLocked 起订阅消费泵（幂等——泵已在即回现有绑定）。mu 持有者
// 调用；投递自订阅时水位起（历史不重推，适配器要历史走 Detail/快照）。
func (g *ChannelGateway) establishLocked(s *session.Session, brief ChannelBrief) *channelBind {
	key := bindKey(brief.Channel, brief.Chat)
	if b, ok := g.binds[key]; ok && b.sub != nil {
		b.s = s
		return b
	}
	sub, seq := s.Subscribe()
	b := &channelBind{brief: brief, s: s, sub: sub, lastID: seq}
	g.binds[key] = b
	cfg, _ := g.cfgOf(brief.Channel)
	g.wg.Add(1)
	go g.pump(b, cfg.Sink)
	return b
}

// attach 会话取回：内存优先；跨重启盘面重建（Reattach 恢复挂起域/历史/
// 排队消息）。nil = 无会话可续（已删除/归属不符）。
func (g *ChannelGateway) attach(brief ChannelBrief) *session.Session {
	if s, ok := g.m.reg.Get(brief.SID); ok {
		return s
	}
	return g.m.reg.Reattach(brief.Owner, brief.SID)
}

// bindOfLocked 绑定激活（mu 持有者调用）：内存绑定 → 会话取回（泵已在）；
// 仅盘面记录（重启后首次触达）→ 取回成功起泵，失败清记录。nil = 无有效绑定。
func (g *ChannelGateway) bindOfLocked(channel, chat string) *channelBind {
	g.loadDiskLocked()
	key := bindKey(channel, chat)
	if b, ok := g.binds[key]; ok {
		if s := g.attach(b.brief); s != nil {
			b.s = s
			return b
		}
		delete(g.binds, key) // 会话已删：绑定失效（新建覆盖在 sessionOf）
		return nil
	}
	rec, ok := g.disk.Binds[key]
	if !ok {
		return nil
	}
	brief := ChannelBrief{Channel: channel, Chat: chat, Owner: rec.Owner, SID: rec.SID}
	s := g.attach(brief)
	if s == nil {
		delete(g.disk.Binds, key)
		g.saveDiskLocked()
		return nil
	}
	return g.establishLocked(s, brief)
}

// sessionOf 绑定会话寻址：有效绑定（内存或盘面）→ 会话取回/起泵；绑定
// 失效或未绑定 → 新建会话 + 落绑定 + 起泵。
func (g *ChannelGateway) sessionOf(msg InboundMsg) (*session.Session, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, errors.New("engine: 渠道编排已关闭")
	}
	if b := g.bindOfLocked(msg.Channel, msg.Chat); b != nil {
		return b.s, nil
	}
	cfg, _ := g.cfgOf(msg.Channel)
	s := g.m.reg.Create(msg.Owner, msg.Text, firstNonEmpty(msg.Mode, contract.ModeManual), contract.UserPrefs{Model: cfg.Model})
	brief := ChannelBrief{Channel: msg.Channel, Chat: msg.Chat, Owner: s.Owner, SID: s.SID}
	g.establishLocked(s, brief)
	g.disk.Binds[bindKey(msg.Channel, msg.Chat)] = bindRecord{SID: s.SID, Owner: s.Owner}
	g.saveDiskLocked()
	return s, nil
}

// Handle 入站消息分流：空闲（ended/error）起轮；运行中/挂起排队（Steer
// ——轮内注入或决议续流后前置带回，不打断执行体）。分流两步 BeginRun/
// Steer 间存在收束竞态，各重试一次；仍失败如实报错。执行体与既有自续轮
// 同款（go Run + noopEmit——出站统一走订阅面，不依赖 fn 生命周期）。
func (g *ChannelGateway) Handle(msg InboundMsg) error {
	if _, ok := g.cfgOf(msg.Channel); !ok {
		return fmt.Errorf("engine: 未注册的渠道 %q（Options.Channels 装配后消息方可路由）", msg.Channel)
	}
	s, err := g.sessionOf(msg)
	if err != nil {
		return err
	}
	mode := firstNonEmpty(msg.Mode, contract.ModeManual)
	for i := 0; i < 2; i++ {
		if s.BeginRun(mode) {
			go g.m.Run(context.Background(), s, msg.Text, msg.Attachments, noopEmit)
			return nil
		}
		if s.Steer(msg.Text, msg.Attachments, mode) {
			return nil
		}
	}
	return errors.New("engine: 渠道消息分流失败（会话状态竞态，可重发）")
}

// Approve 审批/计划决议回写续流（决议端点编排收编：登记 → 回执落流 →
// 落盘 → 续流）。itemID 空 = 单决议/计划卡（plan 档回执走 plan_decision），
// 非空 = 合并决议卡逐项。false = 无挂起可恢复（已续流/超时翻转/并发迟到
// ——幂等拒绝，渠道侧把迟到按钮当已处理）。
func (g *ChannelGateway) Approve(sid, itemID string, d contract.ApprovalDecision) bool {
	s, ok := g.m.reg.Get(sid)
	if !ok {
		return false
	}
	appID := s.PendingAppID()
	if appID == "" {
		return false
	}
	kind, _ := s.PendingDueOf()
	if itemID != "" {
		s.SetDecisionFor(itemID, d)
	} else if items := s.PendingItems(); len(items) > 0 {
		// 合并决议卡「全批/全拒」：逐项登记（恢复流按项领决议），回执与
		// 续流一次（逐项回执是升级位——顶层镜像已可回放重建终态）
		for _, it := range items {
			s.SetDecisionFor(it, d)
		}
	} else {
		s.SetDecision(d)
	}
	if kind == "plan" {
		s.RecordPlanDecision(appID, d)
	} else {
		s.RecordDecision(appID, d)
	}
	g.m.reg.Persist(s)
	go g.m.Resume(context.Background(), s, noopEmit) // 原子抢占归 Resume 首行（BeginResume）
	return true
}

// Answer 提问作答回写续流（ask_user 挂起——语音渠道口头回答经适配器解析
// 后同路）。false 同 Approve（幂等拒绝）。
func (g *ChannelGateway) Answer(sid string, d contract.AskDecision) bool {
	s, ok := g.m.reg.Get(sid)
	if !ok {
		return false
	}
	askID := s.PendingAppID()
	if askID == "" {
		return false
	}
	s.SetAskDecision(d)
	s.RecordAskDecision(askID, d)
	g.m.reg.Persist(s)
	go g.m.Resume(context.Background(), s, noopEmit) // 原子抢占归 Resume 首行（BeginResume）
	return true
}

// Cancel 停当前轮（语音打断/挂断、即时通信停止按钮）：取消执行体，收束
// 走既有中断收尾（interrupted 事件 + 检查点 + 中断注记全落）。挂起态无
// 执行体不打断（等决议或超时兜底）；无绑定/非运行态 false。
func (g *ChannelGateway) Cancel(channel, chat string) bool {
	g.mu.Lock()
	b := g.bindOfLocked(channel, chat)
	g.mu.Unlock()
	if b == nil {
		return false
	}
	if b.s.StateOf() != session.StateRunning {
		return false
	}
	b.s.CancelRun()
	return true
}

// Push 主动推送系统通知（落 harness_note 事件——记录即扇出，消费泵投递
// 渠道侧渲染；不触发运行）。无有效绑定即拒（无投递面）。主动开一轮（定时
// 任务产出等）不经此——通知注入自续是引擎既有语义（NotifyOwner/
// ContinueOrNotify），触发源归应用。
func (g *ChannelGateway) Push(channel, chat, text string) error {
	g.mu.Lock()
	b := g.bindOfLocked(channel, chat)
	g.mu.Unlock()
	if b == nil {
		return fmt.Errorf("engine: 渠道会话未绑定（%s/%s）——无推送面", channel, chat)
	}
	b.s.Record(contract.EvHarnessNote, contract.HarnessNote{Kind: "channel_push", Title: text})
	return nil
}

// Lookup 绑定查询（渠道侧对账/渲染上下文回查；仅读不激活——不动消费泵）。
func (g *ChannelGateway) Lookup(channel, chat string) (ChannelBrief, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadDiskLocked()
	key := bindKey(channel, chat)
	if b, ok := g.binds[key]; ok {
		return b.brief, true
	}
	if rec, ok := g.disk.Binds[key]; ok {
		return ChannelBrief{Channel: channel, Chat: chat, Owner: rec.Owner, SID: rec.SID}, true
	}
	return ChannelBrief{}, false
}

// Unbind 解除绑定（应用删除会话/渠道会话注销时；内存与盘面同摘，消费泵
// 收线时随 stopCh 退出——会话删除后 Record 静默，闲置泵不投递）。
func (g *ChannelGateway) Unbind(channel, chat string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loadDiskLocked()
	key := bindKey(channel, chat)
	_, had := g.binds[key]
	if b, ok := g.binds[key]; ok && b.sub != nil {
		b.s.Unsubscribe(b.sub)
		b.sub = nil
	}
	delete(g.binds, key)
	if _, ok := g.disk.Binds[key]; ok {
		delete(g.disk.Binds, key)
		g.saveDiskLocked()
	}
	return had
}

// Close 渠道编排收线（停机序插在 Registry.Drain 前：先停事件投递面，再收
// 执行体）。幂等；true = 泵组已收净，false = 到点仍有泵在投递（如实上抛，
// 不阻塞停机）。
func (g *ChannelGateway) Close(deadline time.Duration) bool {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return true
	}
	g.closed = true
	for _, b := range g.binds {
		if b.sub != nil {
			b.s.Unsubscribe(b.sub) // 先摘订阅：泵收线前不再收新事件
			b.sub = nil
		}
	}
	close(g.stopCh)
	g.mu.Unlock()

	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(deadline):
		return false
	}
}

// pump 订阅消费泵：事件扇出 → 水位推进 → 渠道投递；投递面尽力而为的对齐
// 兜底两条——事件间隙（慢消费丢事件，订阅通道满即弃的既有语义）即时从
// 快照补投；静默节拍主动对齐真源（被弃的是尾部事件时无「下一事件」触发
// 间隙检测，由节拍追赶收口——终态不缺失）。会话删除后 Record 停记（通道
// 静默），泵闲置至收线。
func (g *ChannelGateway) pump(b *channelBind, sink ChannelSink) {
	defer g.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-g.stopCh:
			return
		case ev := <-b.sub:
			if ev.ID == 0 {
				continue
			}
			if ev.ID <= b.lastID {
				continue // 节拍追赶已投过（通道内滞留的旧事件）
			}
			if ev.ID > b.lastID+1 {
				for _, miss := range b.snapshotBetween(b.lastID, ev.ID) {
					sink.Deliver(b.brief, miss)
				}
			}
			sink.Deliver(b.brief, ev)
			b.lastID = ev.ID
		case <-tick.C:
			for _, miss := range b.catchUp() {
				sink.Deliver(b.brief, miss)
			}
		}
	}
}

// snapshotBetween 事件快照的 (from, to) 开区间切片（间隙补投源）。
func (b *channelBind) snapshotBetween(from, to int) []session.Event {
	var out []session.Event
	for _, ev := range b.s.SnapshotEvents() {
		if ev.ID > from && ev.ID < to {
			out = append(out, ev)
		}
	}
	return out
}

// catchUp 静默追赶：快照中水位之后的全部事件（尾部被弃的收口路径），
// 推进水位（调用方串行——pump 单 goroutine 持有）。
func (b *channelBind) catchUp() []session.Event {
	var out []session.Event
	for _, ev := range b.s.SnapshotEvents() {
		if ev.ID > b.lastID {
			out = append(out, ev)
		}
	}
	if len(out) > 0 {
		b.lastID = out[len(out)-1].ID
	}
	return out
}
