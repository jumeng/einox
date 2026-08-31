package llm

// 错误分类层（网络容错 ③——可重试 vs 致命的单一权威）。
//
// 消费方两处：engine 重试决策（ShouldRetry 取 Retryable）与错误卡装配
// （Code/Message）。分类表对齐 DeepSeek 官方错误码（2026-08-28 用户给定
// api-docs.deepseek.com/zh-cn/quick_start/error_codes）与 anthropic 协议
// type 字符串；openai 协议错误体的业务码细化（智谱 429 族致命码）走
// classifyBizCode；两协议的错误在组件边界已是导出类型（eino-ext openai 组件
// 把 fork APIError 转为组件自有 APIError；claude 组件透传 SDK *anthropic.Error），
// errors.As 穿透包裹链（url.Error/fmt.Errorf %w）即得。
//
// 姿态（codex 对位 + 一处保守偏差）：只对**正向识别**的传输信号重试
// （网络类/429/5xx/空闲哨兵/超时）；未知错误保守放行为致命——eino 默认
// 「任何错误都重试」不采纳（未知持续性失败重试三遍只是拖时间）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
)

// Classified 分类结果（Code 取 contract.ErrorOut 码面字符串字面量——
// llm 不反向依赖 contract，字面量权威在 contract/event.go 注释）。
type Classified struct {
	Retryable bool
	Code      string // SERVER | AUTH | RATE_LIMIT | TRANSPORT
	Message   string // 面向用户的中文文案（调用方自行截断）
}

// Classify 错误分类（nil → 零值；成功路径 ShouldRetry 亦会调用）。
func Classify(err error) Classified {
	if err == nil {
		return Classified{}
	}
	if errors.Is(err, context.Canceled) {
		return Classified{Code: "ABORTED", Message: "已取消"}
	}
	// 超时层哨兵 / ctx 超时：传输类可重试
	if errors.Is(err, ErrIdleTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return Classified{Retryable: true, Code: "TRANSPORT", Message: transportMsg(err)}
	}
	// openai 协议：组件 APIError（业务码细化优先，未命中走 HTTP 状态码面）
	var oe *einoopenai.APIError
	if errors.As(err, &oe) {
		if c, ok := classifyBizCode(oe.Code); ok {
			return c
		}
		return classifyStatus(oe.HTTPStatusCode, detailOf(oe.Message, oe.Error()))
	}
	// anthropic 协议：SDK Error（状态码 + type 字符串双信号）
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		raw := anthropicRaw(ae)
		if c := classifyAnthropicType(string(ae.Type()), raw); c.Code != "" {
			return c
		}
		return classifyStatus(ae.StatusCode, detailOf("", raw))
	}
	// 网络类（dial/reset/timeout——url.Error 包裹下 errors.As 仍穿透）
	var ne net.Error
	if errors.As(err, &ne) {
		return Classified{Retryable: true, Code: "TRANSPORT", Message: transportMsg(err)}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return Classified{Retryable: true, Code: "TRANSPORT", Message: "连接提前关闭：" + truncateErr(err)}
	}
	// 无类型形态的传输错误（http2 GOAWAY / broken pipe 等）
	for _, s := range transportTexts {
		if strings.Contains(err.Error(), s) {
			return Classified{Retryable: true, Code: "TRANSPORT", Message: transportMsg(err)}
		}
	}
	// 未知：保守不重试
	return Classified{Code: "SERVER", Message: truncateErr(err)}
}

var transportTexts = []string{
	"connection reset by peer",
	"broken pipe",
	"unexpected EOF",
	"http2: server sent GOAWAY",
	"use of closed network connection",
}

// classifyStatus HTTP 状态码 → 处置（DeepSeek 官方错误码表对齐）。
func classifyStatus(status int, detail string) Classified {
	switch {
	case status == 401 || status == 403:
		return Classified{Code: "AUTH", Message: "API Key 认证失败（" + itoa(status) + "）——请到模型页检查密钥配置"}
	case status == 402:
		return Classified{Code: "SERVER", Message: "余额不足（402）——请到模型服务商充值后重试"}
	case status == 429:
		return Classified{Retryable: true, Code: "RATE_LIMIT", Message: "请求频率达到上限（429）" + detail}
	case status >= 500:
		return Classified{Retryable: true, Code: "SERVER", Message: "模型服务端故障（" + itoa(status) + "）" + detail}
	default: // 400 格式错 / 422 参数错 / 其余 4xx：修请求才有意义，重试无用
		return Classified{Code: "SERVER", Message: "请求被拒绝（" + itoa(status) + "）" + detail + "——请检查后重试"}
	}
}

// classifyBizCode openai 协议错误体业务码细化（机制厂家中立，码表是数据）：
// 状态码 ≠ 处置语义的缺口只有一处——智谱（GLM）429 族里混着「等待救不活」
// 的欠费/套餐类码（docs.bigmodel.cn 错误码表 1113/1309/1311/1314/1315，均
// 429），状态码面会误报频率上限并空转重试。未命中码一律走状态码面——码表
// 按厂家编号空间天然不冲突（DeepSeek/OpenAI 的 error.code 是语义字符串）。
// 码仅取字符串形态（智谱文档示例形态；数值码不细化退状态码面）。
func classifyBizCode(code any) (Classified, bool) {
	s, _ := code.(string)
	if s == "" {
		return Classified{}, false
	}
	if msg, ok := fatalBizCodes[s]; ok {
		return Classified{Code: "SERVER", Message: msg}, true
	}
	return Classified{}, false
}

// fatalBizCodes 业务码 → 致命处置文案（智谱错误码表，HTTP 均 429——处置
// 对齐 DeepSeek 402 欠费先例：致命停机不降级，文案给可行动指引）。
var fatalBizCodes = map[string]string{
	"1113": "账户已欠费（429/1113）——请到模型服务商充值后重试",
	"1309": "订阅已过期（429/1309）——请续订后重试",
	"1311": "套餐不含该模型（429/1311）——请升级套餐或更换模型",
	"1314": "企业套餐已过期（429/1314）——请联系企业管理员续费",
	"1315": "API Key 类型受限（429/1315）——请更换 Key 类型",
}

// classifyAnthropicType anthropic 协议 type 字符串细化（状态码之外的带内信号；
// 返回零值 Code 表示未命中、回退状态码面）。
func classifyAnthropicType(typ, raw string) Classified {
	switch typ {
	case "authentication_error":
		return Classified{Code: "AUTH", Message: "API Key 认证失败——请到模型页检查密钥配置"}
	case "permission_error", "billing_error", "not_found_error":
		return Classified{Code: "SERVER", Message: "请求被拒绝（" + typ + "）：" + truncateStr(raw)}
	case "rate_limit_error":
		return Classified{Retryable: true, Code: "RATE_LIMIT", Message: "请求频率达到上限（429）"}
	case "overloaded_error", "api_error":
		return Classified{Retryable: true, Code: "SERVER", Message: "模型服务端繁忙：" + truncateStr(raw)}
	}
	return Classified{}
}

// 帮助函数（message 组装的最小面）。

func transportMsg(err error) string {
	return "网络异常（可自动重试）：" + truncateErr(err)
}

// anthropicRaw SDK Error 的安全文案（Error() 在 Request/Response 为 nil 时
// panic——真实运行错误必有，手工构造/边界路径防御性回退 RawJSON）。
func anthropicRaw(ae *anthropic.Error) string {
	if ae == nil || ae.Request == nil || ae.Response == nil {
		return ae.RawJSON()
	}
	return ae.Error()
}

func detailOf(msg, raw string) string {
	if msg != "" {
		return "：" + truncateStr(msg)
	}
	if raw != "" {
		return "：" + truncateStr(raw)
	}
	return ""
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }

func truncateStr(s string) string {
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

func truncateErr(err error) string { return truncateStr(err.Error()) }
