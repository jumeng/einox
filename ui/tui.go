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
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/internal/strutil"
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
	app := &tuiApp{m: m, width: w, height: h, keys: make(chan []byte, 8), redraw: make(chan struct{}, 1),
		out: func(s string) { _, _ = os.Stdout.WriteString(s) }}
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
	defer close(app.quit) // 键盘泵让位收线（选择器错误路径同样覆盖——安全审查 2026-09-06 前挂在 run() 内，pickSession 失败即漏）
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
	pend     []byte // 过滤输入的不完整 UTF-8 前缀（键盘泵每次读 ≤8 字节，CJK rune 可能跨读被切）
	filterIn bool
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
// 词表与 ui/static/index.html 的 DOMAINS/SUMMARIES 同源——改词表须两侧同步
//（跨语言无法机器对账，Go 侧有词表对账测试兜底——tui_test.go）。

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
	// hitl 广前缀（审查 P1-3）：ask_ 涵盖 ask_user_request 与 ask_decision/
	// ask_timeout/ask_ignored（后者前缀是 ask_ 非 ask_user_，两实现曾共同漏）；
	// plan_ 涵盖全部 plan_*——显式枚举新增 Kind 即漏
	case strings.HasPrefix(kind, "approval_") || strings.HasPrefix(kind, "ask_") ||
		strings.HasPrefix(kind, "plan_"):
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
	d, _ := session.EventAs[map[string]any](ev) // 双形态统一取值（typed/map）
	g := func(k string) string { s, _ := d[k].(string); return s }
	switch ev.Event {
	case contract.EvUserMessage:
		who := g("speaker_name")
		if who != "" {
			who = "（" + who + "）"
		}
		text := g("text")
		if text == "" {
			text = "（附件）" // 空文本 = 纯附件消息（与 web 回放页同口径）
		}
		return "消息" + who + "：" + text
	case contract.EvTextDelta:
		return "文本：" + g("delta")
	case contract.EvThinkingDelta:
		return "思考…"
	case contract.EvUsage:
		return "用量上报"
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
	case contract.EvApprovalTimeout:
		return "审批超时（fail-closed 拒）"
	case contract.EvAskRequest:
		return "提问：" + g("question")
	case contract.EvAskDecision:
		// 作答正文：自由文本优先，退选项拼接（活会话 map 内 []string、
		// 线上 JSON 往返 []any 双形态）
		ans := g("free_text")
		if ans == "" {
			switch xs := d["answers"].(type) {
			case []string:
				ans = strings.Join(xs, "、")
			case []any:
				ss := make([]string, 0, len(xs))
				for _, x := range xs {
					if s, ok := x.(string); ok {
						ss = append(ss, s)
					}
				}
				ans = strings.Join(ss, "、")
			}
		}
		if g("decider_name") != "" {
			ans += "（" + g("decider_name") + "）"
		}
		return "作答：" + ans
	case contract.EvAskTimeout:
		return "提问超时"
	case contract.EvAskIgnored:
		return "提问被忽略"
	case contract.EvPlanRequest:
		return "计划：" + g("task")
	case contract.EvPlanDecision:
		v := "拒绝"
		if b, _ := d["approve"].(bool); b {
			v = "批准"
		}
		if g("decider_name") != "" {
			v += "（" + g("decider_name") + "）"
		}
		return v
	case contract.EvPlanTimeout:
		return "计划超时（自动拒）"
	case contract.EvTodoUpdate:
		// 载荷是条目数组（typed []todo.Item）——map 归一不适用，数组同经 EventAs
		if xs, ok := session.EventAs[[]any](ev); ok {
			return fmt.Sprintf("清单 %d 项", len(xs))
		}
		return "清单"
	case contract.EvSteerQueued:
		return "排队：" + g("text")
	case contract.EvSteerUpdated:
		return "排队更新：" + g("text")
	case contract.EvSteerRemoved:
		return "移除排队：" + g("id")
	case contract.EvSteerInjected:
		return "注入对话：" + g("text")
	case contract.EvSteerReordered:
		if xs, ok := d["ids"].([]any); ok {
			return fmt.Sprintf("重排 %d 条", len(xs))
		}
		return "重排"
	case contract.EvNotifyQueued:
		return "通知排队：" + g("text")
	case contract.EvNotifyInjected:
		return "通知注入：" + g("text")
	case contract.EvModelChange:
		return "模型 " + g("from") + " → " + g("to")
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
		// message 截断 60（trunc 边界 n-1 + 省略号；与 web 页 slice(0,60) 略异，
		// 取 TUI trunc 语义一致）
		return "错误 " + g("code") + "：" + strutil.TruncateTotal(g("message"), 60)
	case contract.EvInterrupted:
		return "打断（非故障）"
	case contract.EvTransportRetry:
		n := func(k string) string {
			if v, ok := d[k]; ok && v != nil {
				return fmt.Sprintf("%v", v)
			}
			return "?"
		}
		return "重连 " + n("attempt") + "/" + n("max")
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
	out    func(string) // 输出面（默认 stdout——测试注入捕获；安全审查 2026-09-06 前为方法，渲染不可测即 panic 漏网）
}

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
	defer func() { // 退订兜底：Record 扇出满即弃不阻塞，但僵尸订阅常驻 subs 表（小泄漏）
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
			a.model.pend = nil
		case "del": // 退格（0x7f）——按 rune 截尾（多字节字符截一字节即坏串）
			a.model.pend = nil
			if n := len(a.model.filter); n > 0 {
				_, size := utf8.DecodeLastRuneInString(a.model.filter)
				a.model.filter = a.model.filter[:n-size]
			}
		default:
			// 逐 rune 消费（安全审查 2026-09-06：此前逐字节 string(k) 拼接，
			// CJK 多字节跨读撕裂即产替换符——中文过滤词不可用）；控制字节
			// （<0x20）与坏字节不进过滤器
			a.model.pend = append(a.model.pend, k...)
			for len(a.model.pend) > 0 {
				r, size := utf8.DecodeRune(a.model.pend)
				if r == utf8.RuneError && size == 1 {
					if !utf8.FullRune(a.model.pend) {
						break // 不完整前缀：留待下一批
					}
					a.model.pend = a.model.pend[1:] // 真坏字节：丢弃
					continue
				}
				if r >= 0x20 {
					a.model.filter += string(r)
				}
				a.model.pend = a.model.pend[size:]
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
		line := fmt.Sprintf("  #%d %-18s %s", ev.ID, ev.Event, strutil.TruncateTotal(tuiSummary(ev), a.width-28))
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
		lines := strings.Split(string(payload), "\n")
		n := min(len(lines), max(1, a.height-h-5)) // 安全审查 2026-09-06：载荷行数
		// 可少于可用高度（小载荷常见）——切片前必须钳界，否则越界 panic
		for _, ln := range lines[:n] {
			b.WriteString("  " + strutil.TruncateTotal(ln, a.width-2) + "\x1b[K\r\n")
		}
	}
	b.WriteString("\x1b[38;5;245mk/j 上下 · g 头 G 尾 · Enter 检查器 · / 过滤 · l 实时 · r 刷新 · q 退出\x1b[0m\x1b[K\x1b[K\r\n")
	a.out(b.String())
}
