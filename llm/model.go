package llm

// ChatModel 构造（按 provider.kind 选 eino-ext 组件）+ 思考档位映射。
//
// 思考是用户级档位（effort ∈ {off, low, high, max}，默认 low；2026-08-28
// 三档化取消关档、2026-08-31 关档回归四档——机制层支持四档，具体模型能否
// 真关是业务层的事：各方言只发各自文档的关法，接不接受由端点表态）：
//   anthropic 协议 → 关档不发思考块（协议零思考字段即关）；其余档恒
//     OfEnabled{BudgetTokens} 预算分档（低 8192 / 高 32768 / 最高 =
//     limit.output，见 thinkingBudget）
//   openai 协议 → 思考走方言字段（2026-08-28 泛化）：dialect=deepseek 发
//     thinking:{type:enabled|disabled} + reasoning_effort:low|high|max（DeepSeek
//     私有格式+真实档位名直传；关档 = disabled 且不发 effort，DeepSeek 文档
//     effort 属思考模式）；dialect=glm 同形（智谱 API 的 thinking 枚举同有
//     disabled；GLM-5.3/5.3-Flash 文档限制只能开启——模型层限制由端点报错
//     表态；clear_thinking 只管跨轮历史回传、本仓出站本就剥离故不发送）；
//     dialect=effort 仅 reasoning_effort（通用方言，max→high / off→none
//     对齐 OpenAI 词表——none 为 gpt-5.1+ 官方关思考值）；非方言端点零思考
//     字段零污染（走端点默认）
// 旧值归一（NormalizeEffort）：升级前偏好/存量会话快照里的 on/max → max、
// off 恢复关档本义、""/未知 → 默认 low。
// reasoning_content 回传：两协议组件均**原样透传**、无整形（eino-ext acl/openai
// 出站构造路径无条件拷贝——本仓 08-26 实核订正，旧注「自动处理」有误）；
// 出站剥离归 NewHistoryShapeModel 请求边界包装（H1①，协议定案见
// findings/2026-08-26-h1-probe-reasoning-passback.md），本层不补。
// 图片输入模型（input 含 image）的路由包装归 M3-8，本层先按主模型直连。
//
// 适配铁律（定案见 docs/03 模型面）：「接入」零适配只覆盖方言交集面；
// 多供应商 + 产品级 ⇒ 适配强制（零适配/多供应商/产品级三角最多取二）。
// 差异只经三口收编——Dialect（转形：私有参数）、ModelSpec 元数据（能力
// 对账：有无与档位，适配不无中生有）、Classify（错误归约）；方言词汇表
// 外的能力（Files/Batch/服务端状态）不进统一面。

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	claude "github.com/cloudwego/eino-ext/components/model/claude"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelFactory 模型构造口（测试注入假模型；生产 = NewChatModel）。
type ModelFactory func(ctx context.Context, p ProviderSpec, m ModelSpec, effort string) (model.BaseModel[*schema.Message], error)

// NewChatModel 生产构造：kind → claude / openai 组件（effort 先归一两档）；
// 出口统一挂超时包装（网络容错 ①——全装配面唯一模型构造口，主会话/spawn/
// 拓扑/摘要/genTitle 全覆盖；机制见 timeout.go）。
// responses 协议维持占位（2026-08-28 接真评估定案，证据链见 docs/04：上游
// 仅 agenticopenai 一条路且走 AgenticMessage 族——eino schema 无 ↔Message
// 转换器、需引入官方 openai-go 新依赖、手写双向流式转换层踩上游演进雷区、
// 当前零消费方；OpenAI 官方端点用 openai 协议 chat/completions 可正常接入）。
func NewChatModel(ctx context.Context, p ProviderSpec, m ModelSpec, effort string) (model.BaseModel[*schema.Message], error) {
	effort = NormalizeEffort(effort)
	var cm model.BaseModel[*schema.Message]
	switch p.Kind {
	case "openai":
		c, err := newOpenAIModel(ctx, p, m, effort)
		if err != nil {
			return nil, err
		}
		cm = c
	case "anthropic", "":
		c, err := newClaudeModel(ctx, p, m, effort)
		if err != nil {
			return nil, err
		}
		cm = c
	case "responses":
		return nil, fmt.Errorf("responses 协议暂无 BaseChatModel 组件（上游仅 AgenticModel 形态且无消息转换器）——请改用 openai 协议接入（OpenAI 官方端点 chat/completions 可用）")
	default:
		return nil, fmt.Errorf("未知 provider kind：%s", p.Kind)
	}
	return NewTimeoutModel(cm), nil
}

// NormalizeEffort 思考档归一：off | low | high | max（2026-08-31 关档回归
// 四档——机制层能力，模型是否真支持关由端点定）。旧值兼容——升级前用户
// 偏好与存量会话快照存的是 on/off（旧「开」即 enabled+effort max）：on/
// max → max，off → off（关档回归后恢复本义），其余（""/未知）→ 默认 low。
// 全链路唯一权威：会话读侧（恢复/回显）、API 校验、模型工厂统一走此函数，
// 四档外的值任何一环都不外流。
func NormalizeEffort(effort string) string {
	switch effort {
	case "off":
		return "off"
	case "high":
		return "high"
	case "on", "max":
		return "max"
	default:
		return "low"
	}
}

// defaultOutputTokens 自定义模板默认输出上限（2026-08-28 供应商适配定案
// 显式化——「DeepSeek 参数作默认」的模型规格面）：limit 未知时兜底。
// 数值取 DeepSeek 保守实测；anthropic 协议 MaxTokens 必填（不设即被 API 拒，
// 未知窗口的自定义 Claude 协议模型此前直接不可用——本默认修复该缺口）；
// 思考预算同源钳制（预算 < 输出-1024 协议约束）。openai 协议不设 MaxTokens
// 走端点默认（协议不要求，不发比猜好）。
const defaultOutputTokens = 8192

// thinkingBudget anthropic 协议三档思考预算：低 = 8192 / 高 = 32768 /
// 最高 = 有效输出上限；均不越过 有效输出-1024（协议要求 budget 严格小于
// max_tokens，顶满会被拒）、下限 1024。有效输出 = 显式 limit.output，
// 未知（nil/0）兜自定义模板默认 8192——未知窗口下高档预算同受钳制（宁保守
// 不越协议）。
func thinkingBudget(effort string, m ModelSpec) int64 {
	out := int64(defaultOutputTokens)
	if m.Limit != nil && m.Limit.Output > 0 {
		out = int64(m.Limit.Output)
	}
	b := int64(8192)
	switch effort {
	case "high":
		b = 32768
	case "max":
		b = out
	}
	if hi := out - 1024; b > hi {
		b = hi
	}
	if b < 1024 {
		b = 1024
	}
	return b
}

// intPtr 便捷取址（openai 组件指针字段）。
func intPtr(v int) *int { return &v }

// f32Ptr 采样参数取址（组件字段是 *float32）。
func f32Ptr(v float64) *float32 { f := float32(v); return &f }

// samplingOf 采样参数映射（temperature/top_p）——显式声明才下发，nil = 不发
// 字段走端点默认（多数推理端点拒绝显式 temperature）。两协议组件同款映射，
// 单点锚定供单测断言 nil 不发/显式下发。
func samplingOf(m ModelSpec) (temp, topP *float32) {
	if m.Temperature != nil {
		temp = f32Ptr(*m.Temperature)
	}
	if m.TopP != nil {
		topP = f32Ptr(*m.TopP)
	}
	return
}

// deepseekThinkingFields DeepSeek 思考扩展字段——提为独立函数供单测锚定
// effort → 请求字段映射。开档：闸门 enabled + 三档直传档位名；关档：闸门
// disabled 且不发 reasoning_effort（DeepSeek 文档 effort 属思考模式，关档
// 无意义）。GLM 方言（dialect=glm）线格式同形共用：智谱 API 的 thinking
// 枚举同有 disabled（GLM-5.3/5.3-Flash 文档限制只能开启——模型层限制由
// 端点报错表态），分叉时再拆独立函数。
func deepseekThinkingFields(effort string) map[string]any {
	if effort == "off" {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	}
	return map[string]any{
		"thinking":         map[string]any{"type": "enabled"},
		"reasoning_effort": effort,
	}
}

// effortThinkingFields 通用思考方言（dialect="effort"，2026-08-28 供应商适配
// 定案）：仅 reasoning_effort 一个标准字段、无厂家私有块——覆盖 OpenAI 官方
// 及多数兼容端点的思考控制面。档位映射对齐 OpenAI 词表：low/high 直传、
// max→high（无 max 档端点发未知值必 400，宁降档不发错）、off→none（gpt-5.1+
// 官方关思考值；更早端点不识自会报错——能力归机制、支持归业务）。
func effortThinkingFields(effort string) map[string]any {
	wire := effort
	switch effort {
	case "max":
		wire = "high"
	case "off":
		wire = "none"
	}
	return map[string]any{"reasoning_effort": wire}
}

// newClaudeModel anthropic 协议端点（DeepSeek /anthropic 或 Anthropic 兼容）。
// 思考关档不发思考块（协议零思考字段即关）、其余档显式 OfEnabled（档位即
// 预算档，见 thinkingConfigOf）；MaxTokens 协议必填——显式 limit.output，
// 未知兜自定义模板默认（见 defaultOutputTokens）。
func newClaudeModel(ctx context.Context, p ProviderSpec, m ModelSpec, effort string) (model.BaseModel[*schema.Message], error) {
	cfg := &claude.Config{
		APIKey:  p.APIKey,
		BaseURL: &p.BaseURL,
		Model:   m.ID,
	}
	maxTokens := defaultOutputTokens
	if m.Limit != nil && m.Limit.Output > 0 {
		maxTokens = m.Limit.Output
	}
	cfg.MaxTokens = maxTokens
	cfg.Temperature, cfg.TopP = samplingOf(m) // 显式声明才下发（nil = 不发字段走端点默认）
	cfg.ThinkingConfig = thinkingConfigOf(effort, m)
	return claude.NewChatModel(ctx, cfg)
}

// thinkingConfigOf anthropic 协议思考配置：关档 nil（协议零思考字段即关），
// 其余档 OfEnabled 预算分档（见 thinkingBudget）——单点锚定供单测断言关/开
// 分支（预算数值归 TestThinkingBudget）。
func thinkingConfigOf(effort string, m ModelSpec) *anthropic.ThinkingConfigParamUnion {
	if effort == "off" {
		return nil
	}
	return &anthropic.ThinkingConfigParamUnion{
		OfEnabled: &anthropic.ThinkingConfigEnabledParam{BudgetTokens: thinkingBudget(effort, m)},
	}
}

// newOpenAIModel openai 协议端点（DeepSeek 官方 /chat/completions、内网 vLLM）。
// 思考扩展字段（thinking + reasoning_effort）是 DeepSeek 私有格式，仅
// dialect=deepseek 时发送——厂家扩展绝不写进通用分支（vLLM 等标准端点零污染）。
func newOpenAIModel(ctx context.Context, p ProviderSpec, m ModelSpec, effort string) (model.BaseModel[*schema.Message], error) {
	cfg := &einoopenai.ChatModelConfig{
		APIKey:  p.APIKey,
		BaseURL: p.BaseURL,
		Model:   m.ID,
	}
	if m.Limit != nil && m.Limit.Output > 0 {
		cfg.MaxTokens = intPtr(m.Limit.Output)
	}
	// 采样参数：显式声明才下发（nil = 不发字段走端点默认）
	cfg.Temperature, cfg.TopP = samplingOf(m)
	// 思考方言（厂家/通用知识只走 dialect，绝不写进通用分支——非方言端点
	// 零思考字段，走端点默认）：deepseek = 私有块+档位直传（关档 disabled）；
	// glm = 智谱同形方言（见 deepseekThinkingFields 注）；effort = 通用
	// reasoning_effort（max→high 降档 / off→none）；vLLM 等标准端点零污染不变。
	switch p.Dialect {
	case "deepseek", "glm":
		cfg.ExtraFields = deepseekThinkingFields(effort)
	case "effort":
		cfg.ExtraFields = effortThinkingFields(effort)
	}
	return einoopenai.NewChatModel(ctx, cfg)
}

// GenerateText 单次生成（无头调用辅助——消息构造归基座，应用不触 eino 类型；
// pm local 同步按钮的 commit message 生成路径）。
func GenerateText(ctx context.Context, cm model.BaseModel[*schema.Message], prompt string) (string, error) {
	out, err := cm.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil {
		return "", err
	}
	return out.Content, nil
}
