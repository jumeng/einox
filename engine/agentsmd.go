package engine

// AGENTS.md 注入缝（推通道）：Options.AgentsMD 返回文件清单（绝对路径），
// eino agentsmd 中间件白拿挂载——注入纪律全归上游：transient（注入不进
// 会话历史/检查点）、幂等（Extra 键防重复）、@import 递归（max depth 5）、
// 字节预算（超限文件跳过）、位置在 summarization 之后（注入内容不进摘要
// 基底、不会被压缩掉——上游官方建议位）。发现逻辑（向上遍历/local overlay/
// 用户级+工作区级合并序）归应用清单构造——ZCode 同款双层形态（用户级先、
// 工作区级后收窄覆盖）即由应用把两文件按序放进清单。跨会话记忆的注入通道
// 同走此缝（owner 级记忆文件进清单）。

import (
	"context"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/adk/middlewares/agentsmd"
)

// agentsMDDefaultMaxBytes 注入字节预算缺省（ZCode/codex 同量级保守值；
// AgentsMDMaxBytes 显式设 0 = 用此缺省，不自设无上限——预算失控即提示词
// 失控，dsh「maxBytes 必填显式化」纪律）。
const agentsMDDefaultMaxBytes = 32 * 1024

// agentsmdBackend agentsmd.Backend 的只读 OS 实现（官方仅附 in-memory）。
// 文件不存在包 os.ErrNotExist——上游按可容忍跳过并 OnLoadWarning。
type agentsmdBackend struct{}

func (agentsmdBackend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	data, err := os.ReadFile(req.FilePath)
	if err != nil {
		return nil, err // os.ReadFile 已包 *PathError，errors.Is(err, os.ErrNotExist) 成立
	}
	content := string(data)
	// 行区间语义对齐 skills osBackend（1 基 offset；agentsmd 清单读全量为主）
	if req.Offset > 1 || req.Limit > 0 {
		lines := splitLines(content)
		off := req.Offset
		if off < 1 {
			off = 1
		}
		if off > len(lines) {
			off = len(lines)
		}
		end := len(lines)
		if req.Limit > 0 && off-1+req.Limit < end {
			end = off - 1 + req.Limit
		}
		content = joinLines(lines[off-1 : end])
	}
	return &filesystem.FileContent{Content: content}, nil
}

// splitLines/joinLines 行切分助手（保留末尾换行语义的简单近似——注入面读
// 全量为主，区间读仅上游偏移协议对齐用）。
func splitLines(s string) []string { return strings.Split(s, "\n") }
func joinLines(ls []string) string { return strings.Join(ls, "\n") }

// newAgentsMDMiddleware 清单 → 中间件（空清单/全缺失 = nil 不挂）。
func newAgentsMDMiddleware(ctx context.Context, files []string, maxBytes int) adk.ChatModelAgentMiddleware {
	if len(files) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = agentsMDDefaultMaxBytes
	}
	mw, err := agentsmd.New(ctx, &agentsmd.Config{
		Backend:             agentsmdBackend{},
		AgentsMDFiles:       files,
		AllAgentsMDMaxBytes: maxBytes,
		// OnLoadWarning 走上游缺省日志（预算跳过/文件缺失/超深留痕）——对齐
		// codex tracing::warn 姿态：服务端日志可见，非用户弹窗（dsh 是模型面
		// 注记，本缝注记权在上游中间件，不越俎）。
	})
	if err != nil {
		return nil // 配置面错误不阻断运行（缺省即不注入），装配期已挡掉主要错配
	}
	return mw
}
