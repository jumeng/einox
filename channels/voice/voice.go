// Package voice 流型语音渠道占位（方案接口，零实现——留待后续落地）。
//
// 语音渠道与消息型渠道共用同一编排机制核（engine.ChannelGateway），引擎
// 侧只有文本轮次与事件流——实时媒体处理全部归渠道适配器：
//
//	入站：音频流 → 转写分段（Transcriber）→ Gateway.Handle（文本轮次，
//	      分段时机归适配器——断句/静音检测/固定时长）
//	出站：text_delta 事件流（增量）→ Synthesizer 流式合成 → 音频流出站
//	打断：用户在合成播放中开口 → Gateway.Cancel 停当前轮
//	决议：提问挂起 → 问题文本经合成念出 → 口头回答转写 → Gateway.Answer
//	结束：通话结束 → 会话终态/绑定回收（Gateway.Unbind）
//
// 本包只定义可插拔的转写/合成供应商口（对标 llm 供应商两层模式：内置
// 实现可后补、业务可自定义）；供应商选型与音频格式/采样率/分段策略留待
// 首个实现定稿，接口随实现演进（占位期破坏性变更可接受）。
package voice

import "context"

// Transcriber 流式转写供应商（音频 → 文本段）。一次通话一个流实例。
type Transcriber interface {
	// Transcribe 开流（每次通话调用一次；音频格式约定归实现文档）。
	Transcribe(ctx context.Context) (TranscribeStream, error)
}

// TranscribeStream 转写流：音频块进、文本段出。分段时机归实现——引擎把
// 每段视作一次用户输入（Gateway.Handle 的文本轮次）。
type TranscribeStream interface {
	// SendAudio 音频块入（阻塞至缓冲接纳；实现侧自限内存）。
	SendAudio(pcm []byte) error
	// RecvText 转写段出（io.EOF = 流结束）。
	RecvText() (string, error)
	Close() error
}

// Synthesizer 流式合成供应商（文本增量 → 音频块）。一次出站一段流。
type Synthesizer interface {
	// Synthesize 开流（每段出站文本调用一次；延迟特性归实现文档）。
	Synthesize(ctx context.Context) (SynthesizeStream, error)
}

// SynthesizeStream 合成流：文本增量进、音频块出。text_delta 事件可直接
// 喂入（增量语义对齐——打断时 Close 即停，已出音频丢弃归播放侧）。
type SynthesizeStream interface {
	// SendText 文本增量入。
	SendText(delta string) error
	// RecvAudio 音频块出（io.EOF = 本段合成完毕）。
	RecvAudio() ([]byte, error)
	Close() error
}
