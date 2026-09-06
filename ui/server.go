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
// http.Server 自行包装可加）。
func Serve(m *engine.Manager, cfg Config, addr string) error {
	h, err := New(m, cfg)
	if err != nil {
		return err
	}
	return http.ListenAndServe(addr, h)
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
	owner := r.URL.Query().Get("owner")
	if h.cfg.Authorize != nil {
		if err := h.cfg.Authorize(r, owner, ""); err != nil {
			http.Error(w, "无权访问："+err.Error(), http.StatusForbidden)
			return
		}
	}
	var items []session.SessionListItem
	if owner != "" {
		items = h.m.Registry().List(owner)
	} else {
		items = h.m.Registry().ListAll() // admin 视图（缝内裁剪）
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
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if ev.ID > last {
				writeSSE(w, ev)
				last = ev.ID
				fl.Flush()
			}
		}
	}
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
