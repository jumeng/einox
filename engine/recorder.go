package engine

// 模型调用记录缝装配（批 B；设计件 findings/2026-11-22-model-io-recorder-design.md）：
// Options.ModelRecorder 每会话一实例（SID 记忆化），六面（主/子代理/拓扑/摘要/
// title/review）经 newShapedModel 链尾与 title/autoreview 直调汇点统一挂
// llm.NewRecordingModel——nil 装配零开销直通。子代理/拓扑共享父会话 recorder
// （face 区分）——对齐上游「子调用落父 model-io」语义（querySource=subagent）。

import (
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

// recorderOf 会话记录器（SID 记忆化；nil 装配恒 nil）。工厂返回 nil 同样
// 缓存——该会话恒直通，不重复调用。
func (m *Manager) recorderOf(s *session.Session) llm.ModelRecorder {
	if m.Opt.ModelRecorder == nil {
		return nil
	}
	m.recMu.Lock()
	defer m.recMu.Unlock()
	if r, ok := m.recs[s.SID]; ok {
		return r
	}
	r := m.Opt.ModelRecorder(m.briefOf(s))
	if m.recs == nil {
		m.recs = map[string]llm.ModelRecorder{}
	}
	m.recs[s.SID] = r
	return r
}

// recordModel 记录包装（recorder nil=直通；ident 供 D7 networkEntry 的
// baseURL/transport 对位）。
func (m *Manager) recordModel(s *session.Session, cm model.BaseModel[*schema.Message], ident llm.ModelIdentity, face string) model.BaseModel[*schema.Message] {
	rec := m.recorderOf(s)
	if rec == nil {
		return cm
	}
	return llm.NewRecordingModel(cm, face, ident, rec)
}

// identityOf 记录面模型身份（newShapedModel/title/autoreview 三汇点同构）。
func identityOf(p llm.ProviderSpec, spec llm.ModelSpec, effort string) llm.ModelIdentity {
	return llm.ModelIdentity{ProviderID: p.ID, ModelID: spec.ID, Effort: effort, Kind: p.Kind, BaseURL: p.BaseURL}
}
