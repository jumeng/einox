package ui

// TUI 终端回放浏览器（T9 M3）：进程内直挂 Manager 面——零协议层、单二进制
//（SSH 运维节点直接跑、交叉编译含 loong64——F3 交付故事延伸；dsh 官方无
// TUI，einox 的独有牌）。渲染为手绘 ANSI（读面浏览器不需 TUI 框架——外部
// 依赖面收敛在 x/term 原始模式一件）；交互模型（过滤/滚动/检查器/live）为
// 纯函数可测。读面信任模型为本地进程（鉴权缝属 HTTP 面——TUI 不经网络）。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/session"
)

// TUI 启动终端回放浏览器：sid 空 = 先选会话（ListAll）；否则直入会话视图。
// 键位：k/j 上下 · g 头 G 尾 · Enter 检查器开合 · / 过滤 · l 实时 · r 刷新 ·
// q 退出。单次使用约束（键盘泵 stdin 读阻塞随进程退出——见泵注释）。
func TUI(m *engine.Manager, sid string) error {
	fd := int(os.Stdin.Fd())
	w, h, err := term.GetSize(fd)
	if err != nil {
		return fmt.Errorf("ui: 终端不可用（非 TTY？）：%w", err)
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("ui: 原始模式失败：%w", err)
	}
	defer term.Restore(fd, old)
	app := &tuiApp{m: m, width: w, height: h, keys: make(chan []byte, 8), redraw: make(chan struct{}, 1)}
	app.quit = make(chan struct{})
	go func() { // 键盘泵（原始模式逐字节读，方向键三字节 ESC 序列）。stdin
		// 读阻塞不可选中——退出让位经 keys 满时 select（读阻塞随进程退出，
		// 单次 CLI 使用约束；审查 P2 部分收口）
		buf := make([]byte, 8)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				select {
				case app.keys <- append([]byte(nil), buf[:n]...):
				case <-app.quit:
					return
				}
			}
			if err != nil {
				close(app.keys)
				return
			}
		}
	}()
	if sid == "" {
		picked, err := app.pickSession()
		if err != nil {
			return err
		}
		app.s = picked
	} else if s, ok := m.Registry().Get(sid); ok {
		app.s = s
	} else {
		return fmt.Errorf("ui: 会话 %s 不在内存注册表（冷会话先经应用 Reattach）", sid)
	}
	return app.run()
}

// ── 纯交互模型（可测）──────────────────────────────────────────────────

type tuiModel struct {
	events   []session.Event
	cursor   int // 选中（按可见序）
	offset   int // 视窗首行（可见序）
	filter   string
	filterIn bool // 过滤输入态
	inspOpen bool
	live     bool
}

// matched 过滤命中的事件索引序（空过滤 = 全量）。
func (mo *tuiModel) matched() []int {
	f := strings.ToLower(mo.filter)
	idx := make([]int, 0, len(mo.events))
	for i, ev := range mo.events {
		if f == "" || strings.Contains(ev.Event, f) {
			idx = append(idx, i)
		}
	}
	return idx
}

// move 光标移动（有界；视窗跟随）。
func (mo *tuiModel) move(d, height int) {
	n := len(mo.matched())
	if n == 0 {
		mo.cursor = 0
		return
	}
	mo.cursor = clamp(mo.cursor+d, 0, n-1)
	if mo.cursor < mo.offset {
		mo.offset = mo.cursor
	}
	if mo.cursor >= mo.offset+height {
		mo.offset = mo.cursor - height + 1
	}
}

// append live 事件追加（ID 单调去重——与 SSE 同律）。
func (mo *tuiModel) append(ev session.Event, height int) {
	if len(mo.events) > 0 && ev.ID <= mo.events[len(mo.events)-1].ID {
		return
	}
	mo.events = append(mo.events, ev)
	if mo.live {
		mo.move(1, height) // 跟尾
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

// ── Kind 摘要与域色（与 web 回放页同口径——T6 身份字段渲染）──────────

type tuiDomain int

const (
	domGen tuiDomain = iota
	domTool
	domHitl
	domSteer
	domProc
	domEnd
	domUnknown
)

var domANSI = map[tuiDomain]string{
	domGen: "\x1b[38;5;75m", domTool: "\x1b[38;5;215m", domHitl: "\x1b[38;5;210m",
	domSteer: "\x1b[38;5;79m", domProc: "\x1b[38;5;141m", domEnd: "\x1b[38;5;250m",
	domUnknown: "\x1b[38;5;245m",
}

func tuiDomainOf(kind string) tuiDomain {
	switch {
	case kind == contract.EvTextDelta || kind == contract.EvThinkingDelta || kind == contract.EvUsage:
		return domGen
	case kind == contract.EvToolCall || kind == contract.EvToolResult:
		return domTool
	case strings.HasPrefix(kind, "approval_") || strings.HasPrefix(kind, "ask_user_") ||
		strings.HasPrefix(kind, "plan_request") || strings.HasPrefix(kind, "plan_decision") ||
		strings.HasPrefix(kind, "plan_timeout"):
		return domHitl
	case strings.HasPrefix(kind, "steer_") || strings.HasPrefix(kind, "notify_") ||
		kind == contract.EvUserMessage || kind == contract.EvParticipantUpdate:
		return domSteer
	case kind == contract.EvTodoUpdate || kind == contract.EvHarnessNote || kind == "subagent" ||
		kind == "model_change" || kind == "transport_retry":
		return domProc
	case kind == contract.EvSessionEnd || kind == contract.EvError || kind == "interrupted":
		return domEnd
	}
	return domUnknown
}

// tuiSummary 单行摘要。载荷形态双源：活会话 Record 落的是类型化结构体、
// Reattach 盘面与 HTTP 面是 map——统一归一（map 直用，typed 经 JSON 往返；
// 审查 P1-1：进程内直喂 typed 曾使全部摘要空白）。
func tuiSummary(ev session.Event) string {
	d, _ := ev.Data.(map[string]any)
	if d == nil && ev.Data != nil {
		if b, err := json.Marshal(ev.Data); err == nil {
			_ = json.Unmarshal(b, &d)
		}
	}
	g := func(k string) string { s, _ := d[k].(string); return s }
	switch ev.Event {
	case contract.EvUserMessage:
		who := g("speaker_name")
		if who != "" {
			who = "（" + who + "）"
		}
		return "消息" + who + "：" + g("text")
	case contract.EvTextDelta:
		return "文本：" + g("delta")
	case contract.EvThinkingDelta:
		return "思考…"
	case contract.EvToolCall:
		return "调用 " + g("tool")
	case contract.EvToolResult:
		okf, _ := d["ok"].(bool)
		if okf {
			return "✓ " + g("digest")
		}
		return "✗ " + g("digest")
	case contract.EvApprovalRequest:
		req := ""
		if g("requester_name") != "" {
			req = "（" + g("requester_name") + " 的动作）"
		}
		tgt := ""
		if g("target_name") != "" {
			tgt = " → 问 " + g("target_name")
		}
		return "审批：" + g("tool") + req + tgt
	case contract.EvApprovalDecision:
		v := "拒绝"
		if b, _ := d["approve"].(bool); b {
			v = "批准"
		}
		if g("decider_name") != "" {
			v += "（" + g("decider_name") + "）"
		}
		return v
	case contract.EvHarnessNote:
		return g("kind") + "：" + g("title")
	case "subagent":
		sr := g("stop_reason")
		if sr != "" {
			sr = "（" + sr + "）"
		}
		return "子代理 " + g("kind") + sr
	case contract.EvParticipantUpdate:
		p, _ := d["participant"].(map[string]any)
		name, _ := p["name"].(string)
		if name == "" {
			name, _ = p["id"].(string)
		}
		return "名册：" + name + " " + g("kind")
	case contract.EvSessionEnd:
		return fmt.Sprintf("轮末（历史 %v 条）", d["hist_len"])
	case contract.EvError:
		return "错误 " + g("code") + "：" + g("message")
	}
	return "（未知事件——软降级通用行）"
}

// ── 渲染与主循环 ──────────────────────────────────────────────────────

type tuiApp struct {
	m      *engine.Manager
	s      *session.Session
	model  tuiModel
	sub    chan session.Event
	width  int
	height int
	keys   chan []byte
	quit   chan struct{}
	redraw chan struct{}
}

func (a *tuiApp) out(s string) { _, _ = os.Stdout.WriteString(s) }

func (a *tuiApp) pickSession() (*session.Session, error) {
	items := a.m.Registry().ListAll()
	if len(items) == 0 {
		return nil, fmt.Errorf("ui: 无会话")
	}
	sel := 0
	for {
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J选择会话（↑↓ 移动 · Enter 进入 · q 退出）\r\n")
		for i, it := range items {
			line := fmt.Sprintf("  %s  %s  %s", it.Owner, it.State, it.Title)
			if i == sel {
				b.WriteString("\x1b[7m" + line + "\x1b[0m")
			} else {
				b.WriteString(line)
			}
			b.WriteString("\x1b[K\r\n")
		}
		a.out(b.String())
		k, ok := <-a.keys
		if !ok {
			return nil, fmt.Errorf("ui: 输入关闭")
		}
		switch keyName(k) {
		case "up":
			sel = clamp(sel-1, 0, len(items)-1)
		case "down":
			sel = clamp(sel+1, 0, len(items)-1)
		case "enter":
			s2, ok := a.m.Registry().Get(items[sel].SID)
			if !ok {
				return nil, fmt.Errorf("ui: 会话已不在注册表")
			}
			return s2, nil
		case "q", "ctrlc":
			return nil, fmt.Errorf("ui: 已取消")
		}
	}
}

func keyName(k []byte) string {
	if len(k) == 1 {
		switch k[0] {
		case 13, 10:
			return "enter"
		case 3:
			return "ctrlc"
		case 0x1b:
			return "esc"
		case 0x7f:
			return "del"
		}
		return string(k) // 可打印键保持原样（区分 g/G——ToLower 曾使 G 跳尾不可达，审查 P2）
	}
	if len(k) == 3 && k[0] == 0x1b && k[1] == '[' {
		switch k[2] {
		case 'A':
			return "up"
		case 'B':
			return "down"
		case 'C':
			return "right"
		case 'D':
			return "left"
		}
	}
	return ""
}

func (a *tuiApp) run() error {
	a.model.events = a.s.SnapshotEvents()
	a.out("\x1b[?1049h\x1b[?25l") // 备用屏 + 藏光标
	defer a.out("\x1b[?25h\x1b[?1049l")
	defer close(a.quit) // 键盘泵让位
	defer func() {      // 退订兜底：Record 扇出满即弃不阻塞，但僵尸订阅常驻 subs 表（小泄漏）
		if a.sub != nil {
			a.s.Unsubscribe(a.sub)
		}
	}()
	a.render()
	for {
		select {
		case k, ok := <-a.keys:
			if !ok {
				return nil
			}
			if a.key(k) {
				return nil
			}
		case ev := <-a.subOrNil():
			a.model.append(ev, a.bodyHeight())
		case <-a.redraw:
		}
		a.render()
	}
}

// subOrNil live 订阅（惰性建/断——l 键切换）。
func (a *tuiApp) subOrNil() chan session.Event {
	if !a.model.live {
		if a.sub != nil {
			a.s.Unsubscribe(a.sub)
			a.sub = nil
		}
		return nil
	}
	if a.sub == nil {
		a.sub, _ = a.s.Subscribe()
	}
	return a.sub
}

// key 处理一键；返回 true = 退出。
func (a *tuiApp) key(k []byte) bool {
	name := keyName(k)
	if a.model.filterIn {
		switch name {
		case "enter", "esc":
			a.model.filterIn = false
		case "del": // 退格（0x7f）
			if n := len(a.model.filter); n > 0 {
				a.model.filter = a.model.filter[:n-1]
			}
		default:
			if len(k) == 1 && k[0] >= 0x20 {
				a.model.filter += string(k)
			}
		}
		a.model.cursor, a.model.offset = 0, 0
		return false
	}
	switch name {
	case "q", "ctrlc":
		return true
	case "up", "k":
		a.model.move(-1, a.bodyHeight())
	case "down", "j":
		a.model.move(1, a.bodyHeight())
	case "g":
		a.model.cursor, a.model.offset = 0, 0
	case "G": // 跳尾（keyName 不再 ToLower 后可达）
		a.model.move(len(a.model.matched()), a.bodyHeight())
	case "enter":
		a.model.inspOpen = !a.model.inspOpen
	case "/":
		a.model.filterIn = true
	case "l":
		a.model.live = !a.model.live
	case "r":
		a.model.events = a.s.SnapshotEvents()
	}
	return false
}

func (a *tuiApp) bodyHeight() int {
	h := a.height - 3 // 头 + 尾 + 提示
	if a.model.inspOpen {
		h /= 2
	}
	return max(1, h)
}

func (a *tuiApp) render() {
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	live := "关"
	if a.model.live {
		live = "开"
	}
	filter := a.model.filter
	if a.model.filterIn {
		filter += "▏"
	}
	fmt.Fprintf(&b, "\x1b[1m%s\x1b[0m  %s  实时:%s  过滤:%q  %d/%d\r\n",
		a.s.SID, a.s.StateOf(), live, filter, a.model.cursor+1, len(a.model.matched()))
	idx := a.model.matched()
	h := a.bodyHeight()
	for row := 0; row < h && a.model.offset+row < len(idx); row++ {
		ev := a.model.events[idx[a.model.offset+row]]
		line := fmt.Sprintf("  #%d %-18s %s", ev.ID, ev.Event, trunc(tuiSummary(ev), a.width-28))
		if a.model.offset+row == a.model.cursor {
			b.WriteString("\x1b[7m" + line + "\x1b[0m")
		} else {
			b.WriteString(domANSI[tuiDomainOf(ev.Event)] + line + "\x1b[0m")
		}
		b.WriteString("\x1b[K\r\n")
	}
	if a.model.inspOpen && a.model.cursor < len(idx) {
		ev := a.model.events[idx[a.model.cursor]]
		payload, _ := json.MarshalIndent(ev.Data, "", "  ")
		fmt.Fprintf(&b, "\x1b[38;5;75m#%d %s\x1b[0m\r\n", ev.ID, ev.Event)
		for _, ln := range strings.Split(string(payload), "\n")[:max(1, a.height-h-5)] {
			b.WriteString("  " + trunc(ln, a.width-2) + "\x1b[K\r\n")
		}
	}
	b.WriteString("\x1b[38;5;245mk/j 上下 · g 头 G 尾 · Enter 检查器 · / 过滤 · l 实时 · r 刷新 · q 退出\x1b[0m\x1b[K\x1b[K\r\n")
	a.out(b.String())
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n <= 0 {
		return s
	}
	return string(r[:max(0, n-1)]) + "…"
}
