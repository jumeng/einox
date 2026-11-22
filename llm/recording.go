package llm

// 模型调用记录缝（批 B——D16 留存/D6 轨迹/D7 网络记录/D5 缓存明细的基座地基；
// 设计件 findings/2026-11-22-model-io-recorder-design.md）。NewRecordingModel
// 挂 newShapedModel 链最外层（SystemShape 之外——记录整形后真实出站面），
// Generate/Stream 双实现：每次 attempt 两次投递（started 轻载荷 + 终态全量）
// ——终态一条即上游 model-io 一行语义，started 供 D7 在飞态。
//
// 铁律（上游 runner-debug.ts 对位）：记录面任何故障不得影响模型请求路径——
// 投递全程 recover 兜底、nil recorder 零开销直通；聚合只做 O(len) 追加。
//
// 与上游的登记差异（应用侧登记 FEATURE-MAP）：本缝在传输层之上，不采
// headers/原始 body/providerOptions（天然不落密钥）；attempt 归属=指纹推断
// （同指纹连续调用且上次失败=同请求重试）；WillRetry=Classify+MaxRetries
// 同源推断（非 adk 亲知）。

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/jumeng/einox/internal/shortid"
)

// 记录面 face 常量（六面：主/子代理/拓扑/摘要/标题/评审；应用映射上游
// querySource——main→main_turn、subagent·topology→subagent、summary→compact、
// title→session_title、review→sidecar）。
const (
	FaceMain     = "main"
	FaceSubagent = "subagent"
	FaceTopology = "topology"
	FaceSummary  = "summary"
	FaceTitle    = "title"
	FaceReview   = "review"
)

// 记录投递状态（D7 五态之三+stalled 直得；retry_scheduled=failed+WillRetry
// 由应用合成）。
const (
	RecordStarted   = "started"
	RecordCompleted = "completed"
	RecordFailed    = "failed"
	RecordStalled   = "stalled"
)

// ModelIdentity 调用方模型身份（记录面；BaseURL/Kind 供 D7 networkEntry 的
// baseURL/transport 对位）。
type ModelIdentity struct {
	ProviderID string
	ModelID    string
	Effort     string
	Kind       string
	BaseURL    string
}

// RecordedRequest 出站请求快照（整形后真实发送面——含 system 与工具消息）。
type RecordedRequest struct {
	Messages  []*schema.Message
	ToolNames []string
}

// RecordedUsage 终态用量（缓存双字段=eino v0.9.21+ PromptTokenDetails 透传，
// D5 命中率数据源；ReasoningTokens=CompletionTokensDetails）。
type RecordedUsage struct {
	InputTokens                       int
	OutputTokens                      int
	TotalTokens                       int
	ReasoningTokens                   int
	CacheReadTokens, CacheWriteTokens int
}

// RecordedResponse 终态响应聚合（流式在 chunk 透传侧归并）。
type RecordedResponse struct {
	FinishReason   string
	Text           string
	ReasoningText  string
	ToolCalls      []schema.ToolCall // Index 分片归并（与 runAccum.addToolCall 同语义）
	Usage          *RecordedUsage
	FirstContentIn time.Duration // 首个内容 chunk 时点（上游 timeToFirstContentMs 对位）
}

// RecordedError 终态错误（分类驱动——Code 取 contract.ErrCode* 词表）。
type RecordedError struct {
	Code       string
	Message    string
	StatusCode int // best-effort（0=不可得；openai APIError/anthropic Error 提取）
}

// ModelCallRecord 单次 attempt 记录。
type ModelCallRecord struct {
	Face      string
	RequestID string // 逻辑请求号（重试同号）；shortid
	Attempt   int    // 1 基
	Status    string // RecordStarted|RecordCompleted|RecordFailed|RecordStalled
	StartedAt time.Time
	Duration  time.Duration
	Model     ModelIdentity
	Request   *RecordedRequest // started 投递不带（nil）
	Response  *RecordedResponse
	Err       *RecordedError
	WillRetry bool // failed 终态：推断将重试（Classify.Retryable && attempt≤MaxRetries）
}

// ModelRecorder 模型调用记录器（每会话一实例——Options.ModelRecorder 构造；
// 实现自行保证并发安全；任何 panic 被包装器兜底不影响模型路径）。
type ModelRecorder interface {
	Record(call ModelCallRecord)
}

// NewRecordingModel 记录包装（rec=nil 直通零开销）。ident 为调用方模型身份。
func NewRecordingModel(inner model.BaseModel[*schema.Message], face string, ident ModelIdentity, rec ModelRecorder) model.BaseModel[*schema.Message] {
	return &recordingModel{inner: inner, face: face, ident: ident, rec: rec}
}

type recordingModel struct {
	inner model.BaseModel[*schema.Message]
	face  string
	ident ModelIdentity
	rec   ModelRecorder

	mu       sync.Mutex
	lastFP   uint64 // 上次调用指纹（attempt 归属推断）
	lastID   string
	lastAtt  int
	lastFail bool
}

// begin attempt 开题：指纹比对定 RequestID/attempt（同指纹且上次失败=重试
// 续号；否则新请求）。返回 (requestID, attempt, startedAt)。
func (r *recordingModel) begin(fp uint64) (string, int, time.Time) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if fp == r.lastFP && r.lastFail {
		r.lastAtt++
	} else {
		r.lastID = shortid.Hex("req", 8)
		r.lastAtt = 1
	}
	r.lastFP = fp
	return r.lastID, r.lastAtt, now
}

// end attempt 终态登记（失败标记供下次指纹比对）。
func (r *recordingModel) end(failed bool) {
	r.mu.Lock()
	r.lastFail = failed
	r.mu.Unlock()
}

func (r *recordingModel) deliver(rec ModelCallRecord) {
	if r.rec == nil {
		return
	}
	defer func() { _ = recover() }() // 铁律：记录失败绝不影响模型路径
	r.rec.Record(rec)
}

// requestOf 调用现场快照（消息浅拷贝防下游并发改写；toolNames 经 options 提取）。
func requestOf(input []*schema.Message, opts ...model.Option) *RecordedRequest {
	req := &RecordedRequest{Messages: append([]*schema.Message(nil), input...)}
	if o := model.GetCommonOptions(&model.Options{}, opts...); o != nil {
		for _, ti := range append(append([]*schema.ToolInfo(nil), o.Tools...), o.DeferredTools...) {
			if ti != nil {
				req.ToolNames = append(req.ToolNames, ti.Name)
			}
		}
	}
	return req
}

func (r *recordingModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	req := requestOf(input, opts...)
	id, att, started := r.begin(fingerprint(input, opts...))
	r.deliver(ModelCallRecord{Face: r.face, RequestID: id, Attempt: att, Status: RecordStarted,
		StartedAt: started, Model: r.ident})
	out, err := r.inner.Generate(ctx, input, opts...)
	r.finish(id, att, started, req, out, err)
	return out, err
}

func (r *recordingModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	req := requestOf(input, opts...)
	id, att, started := r.begin(fingerprint(input, opts...))
	r.deliver(ModelCallRecord{Face: r.face, RequestID: id, Attempt: att, Status: RecordStarted,
		StartedAt: started, Model: r.ident})
	sr, err := r.inner.Stream(ctx, input, opts...)
	if err != nil {
		r.finish(id, att, started, req, nil, err)
		return sr, err
	}
	// 透传泵（Pipe 转发+同步聚合，StreamReader 结构体不可直接实现故走协程）：
	// 消费侧逐 chunk 照收，EOF/错误原样透传；终态在泵收线时投递——正常收线/
	// 真实错误/消费侧提前 Close 三路都到 settle，不漏终态。
	pr, pw := schema.Pipe[*schema.Message](8)
	go func() {
		defer pw.Close()
		var agg recordAgg
		var firstAt time.Time
		for {
			chunk, err := sr.Recv()
			if err != nil {
				// io.EOF 归一为 nil（正常收线=completed；Classify 把 EOF 归可重试
				// 传输类，直传会被误记 failed）。先投递终态再透传错误——消费方
				// 观察到错误/EOF 时终态记录必已就位（顺序契约）。
				if errors.Is(err, io.EOF) {
					err = nil
				}
				r.settle(id, att, started, req, &agg, firstAt, err)
				if err != nil {
					pw.Send(nil, err) // 真实错误透传（重试哨兵/超时/取消由消费侧分类）
				}
				return
			}
			if chunk != nil {
				absorb(&agg, &firstAt, started, chunk)
				if pw.Send(chunk, nil) {
					sr.Close()
					r.settle(id, att, started, req, &agg, firstAt, context.Canceled)
					return // 消费侧关线=中止（会话停止/删除），终态按 aborted 记
				}
			}
		}
	}()
	return pr, nil
}

// finish 终态聚合投递（Generate 返回/Stream 首错共用；out=nil+err=nil 理论
// 不可达，防御按 completed 空响应记）。
func (r *recordingModel) finish(id string, att int, started time.Time, req *RecordedRequest, out *schema.Message, err error) {
	call := ModelCallRecord{Face: r.face, RequestID: id, Attempt: att,
		StartedAt: started, Duration: time.Since(started), Model: r.ident, Request: req}
	switch {
	case err == nil:
		call.Status = RecordCompleted
		call.Response = responseOf(out)
	default:
		c := Classify(err)
		call.Status = RecordFailed
		call.Err = &RecordedError{Code: c.Code, Message: err.Error(), StatusCode: statusCodeOf(err)}
		if errors.Is(err, ErrIdleTimeout) {
			call.Status = RecordStalled // 流式空闲哨兵（上游 stream stalled 对位）
		}
		call.WillRetry = c.Retryable && att <= MaxRetries
	}
	r.end(call.Status != RecordCompleted)
	r.deliver(call)
}

// recordAgg 流式 chunk 聚合态（透传泵内私有，单协程无锁）。
type recordAgg struct {
	text, reasoning, finish string
	toolCalls               []schema.ToolCall
	tcSlots                 map[int]int
	usage                   *schema.TokenUsage
}

// absorb chunk 归并（toolCall Index 分片与 runAccum.addToolCall 同语义）。
func absorb(agg *recordAgg, firstAt *time.Time, started time.Time, m *schema.Message) {
	if m == nil {
		return
	}
	if firstAt.IsZero() && (m.Content != "" || m.ReasoningContent != "") {
		*firstAt = time.Now()
	}
	agg.text += m.Content
	agg.reasoning += m.ReasoningContent
	if m.ResponseMeta != nil {
		if m.ResponseMeta.FinishReason != "" {
			agg.finish = m.ResponseMeta.FinishReason
		}
		if m.ResponseMeta.Usage != nil {
			agg.usage = m.ResponseMeta.Usage
		}
	}
	for _, tc := range m.ToolCalls {
		if tc.Index == nil { // 非分片形态（完整调用）：直接追加
			agg.toolCalls = append(agg.toolCalls, tc)
			continue
		}
		if pos, ok := agg.tcSlots[*tc.Index]; ok {
			merged := &agg.toolCalls[pos]
			if tc.ID != "" {
				merged.ID = tc.ID
			}
			if tc.Type != "" {
				merged.Type = tc.Type
			}
			if tc.Function.Name != "" {
				merged.Function.Name = tc.Function.Name
			}
			merged.Function.Arguments += tc.Function.Arguments
			continue
		}
		agg.toolCalls = append(agg.toolCalls, tc)
		if agg.tcSlots == nil {
			agg.tcSlots = map[int]int{}
		}
		agg.tcSlots[*tc.Index] = len(agg.toolCalls) - 1
	}
}

// settle 流式终态投递（EOF=completed；错误=failed/stalled；消费侧关线=
// aborted 记 failed）。
func (r *recordingModel) settle(id string, att int, started time.Time, req *RecordedRequest, agg *recordAgg, firstAt time.Time, err error) {
	firstIn := time.Duration(0)
	if !firstAt.IsZero() {
		firstIn = firstAt.Sub(started)
	}
	if err == nil {
		r.end(false)
		r.deliver(ModelCallRecord{Face: r.face, RequestID: id, Attempt: att,
			Status: RecordCompleted, StartedAt: started, Duration: time.Since(started),
			Model: r.ident, Request: req,
			Response: &RecordedResponse{
				FinishReason: agg.finish, Text: agg.text, ReasoningText: agg.reasoning,
				ToolCalls: agg.toolCalls, Usage: usageOf(agg.usage), FirstContentIn: firstIn,
			}})
		return
	}
	c := Classify(err)
	status := RecordFailed
	if errors.Is(err, ErrIdleTimeout) {
		status = RecordStalled
	}
	r.end(true)
	r.deliver(ModelCallRecord{Face: r.face, RequestID: id, Attempt: att,
		Status: status, StartedAt: started, Duration: time.Since(started),
		Model: r.ident, Request: req,
		Err:       &RecordedError{Code: c.Code, Message: err.Error(), StatusCode: statusCodeOf(err)},
		WillRetry: c.Retryable && att <= MaxRetries})
}

// responseOf 非流式终态聚合（FirstContentIn 非流式不可得=0——rounds 的 tps
// 口径仅流式有值，与上游 timeToFirstContentMs 同位）。
func responseOf(out *schema.Message) *RecordedResponse {
	resp := &RecordedResponse{}
	if out == nil {
		return resp
	}
	resp.Text = out.Content
	resp.ReasoningText = out.ReasoningContent
	resp.ToolCalls = out.ToolCalls
	if out.ResponseMeta != nil {
		resp.FinishReason = out.ResponseMeta.FinishReason
		resp.Usage = usageOf(out.ResponseMeta.Usage)
	}
	return resp
}

// usageOf 终态用量归一（nil=不可得；缓存双字段自 PromptTokenDetails）。
func usageOf(u *schema.TokenUsage) *RecordedUsage {
	if u == nil {
		return nil
	}
	return &RecordedUsage{
		InputTokens:      u.PromptTokens,
		OutputTokens:     u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  u.CompletionTokensDetails.ReasoningTokens,
		CacheReadTokens:  u.PromptTokenDetails.CachedTokens,
		CacheWriteTokens: u.PromptTokenDetails.CacheWriteTokens,
	}
}

// statusCodeOf best-effort HTTP 状态码提取（组件导出类型 errors.As 穿透包裹链）。
func statusCodeOf(err error) int {
	var oe *einoopenai.APIError
	if errors.As(err, &oe) {
		return oe.HTTPStatusCode
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// fingerprint 调用指纹（attempt 归属推断：fnv64a over 消息序列+工具名——
// 整形后出站面逐项入哈希，网络调用前 O(len) 可承受）。
func fingerprint(input []*schema.Message, opts ...model.Option) uint64 {
	h := fnv.New64a()
	var write = func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(strconv.Itoa(len(input)))
	for _, m := range input {
		if m == nil {
			write("nil")
			continue
		}
		write(string(m.Role))
		write(m.Content)
		write(m.ReasoningContent)
		for _, tc := range m.ToolCalls {
			write(tc.ID)
			write(tc.Function.Name)
			write(tc.Function.Arguments)
		}
	}
	if o := model.GetCommonOptions(&model.Options{}, opts...); o != nil {
		for _, ti := range o.Tools {
			if ti != nil {
				write(ti.Name)
			}
		}
	}
	return h.Sum64()
}
