package ui

// 交互面（T9 M2）：控制端点——run（Handle 同款分流：空闲起轮/运行中排队）、
// resume/stop、决议（approve/answer——decider 携带点按钮的人，与 T6 身份链
// 及 DecisionGuard 校验面一致）、参与者名册。幂等迟到（ErrNoPendingDecision）
// 映射 409——前端把迟到按钮当已处理。

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/session"
)

func (h *server) mountControl(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/sessions/{sid}/run", h.run)
	mux.HandleFunc("POST /api/sessions/{sid}/resume", h.resume)
	mux.HandleFunc("POST /api/sessions/{sid}/stop", h.stop)
	mux.HandleFunc("POST /api/sessions/{sid}/approve", h.approve)
	mux.HandleFunc("POST /api/sessions/{sid}/answer", h.answer)
	mux.HandleFunc("GET /api/sessions/{sid}/participants", h.participants)
	mux.HandleFunc("POST /api/sessions/{sid}/participants", h.upsertParticipant)
}

// runReq 起轮请求。Speaker 可选（nil/空 = 单用户语义——T6 零变化）。
type runReq struct {
	Text        string               `json:"text"`
	SpeakerID   string               `json:"speaker_id,omitempty"`
	SpeakerName string               `json:"speaker_name,omitempty"`
	Attachments []session.Attachment `json:"attachments,omitempty"`
}

// run 起轮/排队（ChannelGateway.Handle 同款编排：BeginRun 成功起轮，运行中/
// 挂起转排队——SteerBy 带说话人；说话人首见登记名册，与渠道入站同律）。
func (h *server) run(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	var in runReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON："+err.Error(), http.StatusBadRequest)
		return
	}
	var actor *contract.Participant
	if in.SpeakerID != "" {
		actor = &contract.Participant{ID: in.SpeakerID, Name: in.SpeakerName}
		s.UpsertParticipant(*actor)
	}
	mode := s.ModePublic()
	if mode == "" {
		mode = contract.ModeManual
	}
	for i := 0; i < 2; i++ { // 收束竞态重试一次（Handle 同款）
		if s.BeginRun(mode) {
			if actor != nil {
				s.SetTurnActor(actor)
			}
			go h.m.Run(r.Context(), s, in.Text, in.Attachments, func(session.Event) {})
			writeJSON(w, map[string]any{"ok": true, "started": true})
			return
		}
		if s.SteerBy(actor, in.Text, in.Attachments, mode) {
			writeJSON(w, map[string]any{"ok": true, "queued": true})
			return
		}
	}
	http.Error(w, "起轮分流失败（会话状态竞态，可重试）", http.StatusConflict)
}

func (h *server) resume(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	go h.m.Resume(r.Context(), s, func(session.Event) {}) // 原子抢占归 Resume 首行（BeginResume）
	writeJSON(w, map[string]any{"ok": true})
}

func (h *server) stop(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	s.CancelRun() // 空转安全（无执行体即 no-op）
	writeJSON(w, map[string]any{"ok": true})
}

// approveReq 决议请求。Decider 可选（空 = 匿名决议——DecisionGuard 启用时
// 卡有目标且决议带人才校验，与 Channels().Approve 语义一致）。
type approveReq struct {
	Approve     bool   `json:"approve"`
	Reason      string `json:"reason,omitempty"`
	ItemID      string `json:"item_id,omitempty"`
	DeciderID   string `json:"decider_id,omitempty"`
	DeciderName string `json:"decider_name,omitempty"`
}

func (h *server) approve(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if !h.authorizeSID(w, r, sid) {
		return
	}
	var in approveReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON："+err.Error(), http.StatusBadRequest)
		return
	}
	var decider *contract.Participant
	if in.DeciderID != "" {
		decider = &contract.Participant{ID: in.DeciderID, Name: in.DeciderName}
	}
	d := contract.ApprovalDecision{Approve: in.Approve, Reason: in.Reason,
		DeciderID: in.DeciderID, DeciderName: in.DeciderName}
	err := h.m.Channels().Approve(sid, in.ItemID, decider, d)
	switch {
	case err == nil:
		writeJSON(w, map[string]any{"ok": true})
	case errors.Is(err, engine.ErrNoPendingDecision):
		http.Error(w, err.Error(), http.StatusConflict) // 幂等迟到——前端当已处理
	default:
		http.Error(w, err.Error(), http.StatusForbidden) // DecisionGuard 拒绝等
	}
}

// answerReq 提问作答请求。
type answerReq struct {
	Answers     []string `json:"answers,omitempty"`
	FreeText    string   `json:"free_text,omitempty"`
	DeciderID   string   `json:"decider_id,omitempty"`
	DeciderName string   `json:"decider_name,omitempty"`
}

func (h *server) answer(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if !h.authorizeSID(w, r, sid) {
		return
	}
	var in answerReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "请求体不是合法 JSON："+err.Error(), http.StatusBadRequest)
		return
	}
	var decider *contract.Participant
	if in.DeciderID != "" {
		decider = &contract.Participant{ID: in.DeciderID, Name: in.DeciderName}
	}
	if !h.m.Channels().Answer(sid, decider, contract.AskDecision{
		Answers: in.Answers, FreeText: in.FreeText,
		DeciderID: in.DeciderID, DeciderName: in.DeciderName}) {
		http.Error(w, "会话无挂起提问（已处理或超时）", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *server) participants(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	writeJSON(w, s.ParticipantsOf())
}

func (h *server) upsertParticipant(w http.ResponseWriter, r *http.Request) {
	s := h.sessionOf(w, r)
	if s == nil {
		return
	}
	var p contract.Participant
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.ID == "" {
		http.Error(w, "参与者需非空 id", http.StatusBadRequest)
		return
	}
	kind := s.UpsertParticipant(p)
	writeJSON(w, map[string]any{"ok": true, "kind": kind})
}

// authorizeSID 决议类端点的鉴权缝（不取会话体——无挂起时也要先过缝；
// owner 经注册表解析，无会话按 404 让位）。
func (h *server) authorizeSID(w http.ResponseWriter, r *http.Request, sid string) bool {
	if h.cfg.Authorize == nil {
		return true
	}
	s, ok := h.m.Registry().Get(sid)
	if !ok {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return false
	}
	if err := h.cfg.Authorize(r, s.Owner, sid); err != nil {
		http.Error(w, "无权访问："+err.Error(), http.StatusForbidden)
		return false
	}
	return true
}
