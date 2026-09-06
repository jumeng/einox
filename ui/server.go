// Package ui 是官方通用界面件（T9 装配式默认界面的服务端件）：只读回放面
// （REST + SSE）+ embed 单页回放组件。装配先例同 channels/——应用 import
// 才进构建；零引擎改动（Registry/Session 公开面的纯装配）。
//
// 装配面：ui.New(m, Config) → http.Handler（应用自挂 mux）；ui.Serve(addr)
// 便捷档阻塞监听。
// 扩展点：Config.Authorize 鉴权缝（nil = 不校验——多租户/会话可见性归应用；
// owner 从注册表解析非 URL 参数，不可伪造）。
// 已知限制：单会话面寻址内存注册表——冷会话（仅盘面）先经应用 Reattach；
// live 档依赖进程内会话（跨进程走应用网关）。前端工具链不进 go build
// （M1 原生 JS 起步；React 化与 TS 类型生成是 M2 的有意决策，评审可见）。
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/session"
)

//go:embed static
var staticFiles embed.FS

// Config uiserver 配置（零值可用——鉴权缝 nil = 不校验）。
type Config struct {
	// Authorize 请求鉴权缝（nil = 不校验）。单会话面的 owner 取目标会话
	// 实际归属；列画面 owner 取 ?owner= 查询参数（空 = 全量 admin 视图，
	// 缝内自行裁剪）。
	Authorize func(r *http.Request, owner, sid string) error
}

// Serve 便捷档：构造 + 阻塞监听（独立端口跑回放面；优雅停机归应用——
// http.Server 自行包装可加）。ReadHeaderTimeout 防慢连接资源占用（裸
// ListenAndServe 的零值 Server 无超时——安全审查 2026-09-06）。
func Serve(m *engine.Manager, cfg Config, addr string) error {
	h, err := New(m, cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 15 * time.Second}
	return srv.ListenAndServe()
}

// New 构造服务（读面 + M2 控制面同包装配——control.go）。端点：GET /api/sessions?owner=（列表，空 = 全量）；
// GET /api/sessions/{sid}?since=（详情 + 增量事件）；GET /api/sessions/{sid}/
// events?since=&live=1（默认回放档 = 快照 JSON 数组〔golden 同款形态〕；
// live=1 = SSE：先快照补齐再实时转发，按事件 ID 去重，断线重连带 since 续传）。
func New(m *engine.Manager, cfg Config) (http.Handler, error) {
	if m == nil {
		return nil, errors.New("ui: Manager 不可为 nil")
	}
	h := &server{m: m, cfg: cfg}
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", h.list)
	mux.HandleFunc("GET /api/sessions/{sid}", h.detail)
	mux.HandleFunc("GET /api/sessions/{sid}/events", h.events)
	h.mountControl(mux)
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return mux, nil
}

type server struct {
	m   *engine.Manager
	cfg Config
}

func (h *server) list(w http.ResponseWriter, r *http.Request) {
	// owner 过滤走「在册身份查表」而非路径派生（安全审查 2026-09-06：
	// ?owner= 此前直达存储层 users/<op>/ 路径构造——查询参数现在只用于与
	// 服务端枚举的在册用户精确比较，进 List 的值恒来自 ListUsers 枚举，
	// 请求原文不再触达任何路径构造；session.ValidOwner 与存储层围栏保留
	// 为纵深）。无匹配（未知/非法形态/暂未落盘）= 空清单。
	q := r.URL.Query().Get("owner")
	if h.cfg.Authorize != nil {
		if err := h.cfg.Authorize(r, q, ""); err != nil {
			http.Error(w, "无权访问："+err.Error(), http.StatusForbidden)
			return
		}
	}
	var items []session.SessionListItem
	if q == "" {
		items = h.m.Registry().ListAll() // admin 视图（缝内裁剪）
	} else {
		items = []session.SessionListItem{} // 无匹配保持 [] wire 形态（与 List 空态一致）
		for _, u := range h.m.Registry().Store().ListUsers() {
			if u == q {
				items = h.m.Registry().List(u)
				break
			}
		}
	}
	writeJSON(w, items)
}

func (h *server) detail(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	d, ok := h.m.Registry().Detail(s.SID, since)
	if !ok {
		http.Error(w, "会话详情不可用", http.StatusNotFound)
		return
	}
	writeJSON(w, d)
}

// events 事件面：回放档（快照 JSON——T7 golden 基线同款形态，四场景可作
// 本端点的测试夹具）与 live 档（SSE）。live 语义：先订阅（取当前序号）后
// 快照——两源重叠段按事件 ID 去重，无洞无缝。
func (h *server) events(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	if r.URL.Query().Get("live") != "1" {
		evs := s.SnapshotEvents()
		out := make([]session.Event, 0, len(evs))
		for _, ev := range evs {
			if ev.ID > since {
				out = append(out, ev)
			}
		}
		writeJSON(w, out)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "流式面不可用", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch, _ := s.Subscribe()
	defer s.Unsubscribe(ch)
	last := since
	for _, ev := range s.SnapshotEvents() {
		if ev.ID > since {
			writeSSE(w, ev)
			last = ev.ID
		}
	}
	fl.Flush()
	// 慢消费兜底（与 engine/channel.go 消费泵同律——订阅通道缓冲 64 满即弃）：
	// ①到达事件与水位有间隙 → 快照补投（gapFill）；②250ms 节拍追赶——被弃的
	// 是尾部事件时无「下一事件」触发间隙检测，由节拍收口（终态不缺失）。
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev.ID == 0 || ev.ID <= last {
				continue
			}
			for _, miss := range gapFill(s.EventsSince(last), last, ev.ID) {
				writeSSE(w, miss)
				last = miss.ID
			}
			writeSSE(w, ev)
			last = ev.ID
			fl.Flush()
		case <-tick.C:
			for _, miss := range s.EventsSince(last) {
				writeSSE(w, miss)
				last = miss.ID
			}
			fl.Flush()
		}
	}
}

// gapFill 间隙补投决策（纯函数）：快照切片中 (last, evID) 开区间的事件——
// 到达事件证明水位与它之间有被弃事件（订阅满即弃），按序补投在前。
func gapFill(snap []session.Event, last, evID int) []session.Event {
	var out []session.Event
	for _, e := range snap {
		if e.ID > last && e.ID < evID {
			out = append(out, e)
		}
	}
	return out
}

// sessionOf 单会话寻址 + 鉴权缝（owner 从注册表解析——不可经 URL 伪造）。
func (h *server) sessionOf(w http.ResponseWriter, r *http.Request) *session.Session {
	s, ok := h.m.Registry().Get(r.PathValue("sid"))
	if !ok {
		http.Error(w, "会话不存在（内存注册表；冷会话先经应用 Reattach）", http.StatusNotFound)
		return nil
	}
	if h.cfg.Authorize != nil {
		if err := h.cfg.Authorize(r, s.Owner, s.SID); err != nil {
			http.Error(w, "无权访问："+err.Error(), http.StatusForbidden)
			return nil
		}
	}
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, ev session.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
}
