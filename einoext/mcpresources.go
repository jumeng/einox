package einoext

// MCP resources 面（N2，dsh PR #4082/#4106 形态对位）：
//   资源不预载不注入——连接声明 resources 能力才挂三工具（list/templates/
//   read 按需取，读结果二进制 blob 不进模型上下文）；server instructions
//   经握手捕获、MCPSection() 供应用 Instruction 拼装（机制与内容分离——
//   放置权归应用）；32KiB 上限，超限整个来源跳过（fail-closed 于提示注入
//   面——dsh 同款「超限连接失败」语义）；未配置/未声明 = 零工具零指令
//   零 token。工具命名对齐 dsh（list_mcp_resources 族——不带 mcp_ 前缀，
//   不落入「远端语义未知按写工具审批」的既有约定：资源面是已知只读语义）。

import (
	"context"
	"fmt"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// maxMCPInstructionBytes server instructions 上限（dsh 默认 32768 同源）。
const maxMCPInstructionBytes = 32 << 10

// mcpResourceClient 资源面所需的最小 client 面（*client.Client 天然满足；
// 窄接口为测试留缝）。
type mcpResourceClient interface {
	ListResources(ctx context.Context, request mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error)
	ListResourceTemplates(ctx context.Context, request mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error)
	ReadResource(ctx context.Context, request mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error)
}

// newResourceTools 资源三工具（serverName 进描述与错误文案——模型可辨识
// 来源；构造失败属装配错误，静默跳过与工具面同容错度）。
func newResourceTools(rc mcpResourceClient, serverName string) []contract.Tool {
	var out []contract.Tool
	if t, err := tools.InferTool("list_mcp_resources",
		"列出 MCP 服务器 "+serverName+" 的资源清单（名称/URI/MIME/描述）。按需浏览，不自动装载。",
		func(ctx context.Context, in struct {
			Cursor string `json:"cursor,omitempty"` // 续页游标（首页省略）
		}) (map[string]any, error) {
			req := mcp.ListResourcesRequest{}
			req.Params.Cursor = mcp.Cursor(in.Cursor)
			res, err := rc.ListResources(ctx, req)
			if err != nil {
				return tools.Fail("MCP 资源列举失败：" + err.Error()), nil
			}
			out := map[string]any{"ok": true, "server": serverName}
			list := make([]map[string]any, 0, len(res.Resources))
			for _, r := range res.Resources {
				list = append(list, map[string]any{
					"name": r.Name, "uri": r.URI, "mime": r.MIMEType, "description": r.Description,
				})
			}
			out["resources"] = list
			if res.NextCursor != "" {
				out["next_cursor"] = string(res.NextCursor)
			}
			return out, nil
		}); err == nil {
		out = append(out, t)
	}
	if t, err := tools.InferTool("list_mcp_resource_templates",
		"列出 MCP 服务器 "+serverName+" 的资源模板（参数化 URI 形态）。",
		func(ctx context.Context, in struct {
			Cursor string `json:"cursor,omitempty"`
		}) (map[string]any, error) {
			req := mcp.ListResourceTemplatesRequest{}
			req.Params.Cursor = mcp.Cursor(in.Cursor)
			res, err := rc.ListResourceTemplates(ctx, req)
			if err != nil {
				return tools.Fail("MCP 模板列举失败：" + err.Error()), nil
			}
			out := map[string]any{"ok": true, "server": serverName}
			list := make([]map[string]any, 0, len(res.ResourceTemplates))
			for _, r := range res.ResourceTemplates {
				list = append(list, map[string]any{
					"name": r.Name, "uri_template": r.URITemplate.Raw(), "mime": r.MIMEType, "description": r.Description,
				})
			}
			out["templates"] = list
			if res.NextCursor != "" {
				out["next_cursor"] = string(res.NextCursor)
			}
			return out, nil
		}); err == nil {
		out = append(out, t)
	}
	if t, err := tools.InferTool("read_mcp_resource",
		"读取 MCP 服务器 "+serverName+" 的一个资源（uri 取自 list_mcp_resources）。文本原样返回；二进制只报类型与大小。",
		func(ctx context.Context, in struct {
			URI string `json:"uri"`
		}) (map[string]any, error) {
			if in.URI == "" {
				return tools.Fail("uri 不能为空——先 list_mcp_resources 浏览"), nil
			}
			req := mcp.ReadResourceRequest{}
			req.Params.URI = in.URI
			res, err := rc.ReadResource(ctx, req)
			if err != nil {
				return tools.Fail("MCP 资源读取失败：" + err.Error()), nil
			}
			out := map[string]any{"ok": true, "server": serverName, "uri": in.URI}
			contents := make([]map[string]any, 0, len(res.Contents))
			for _, c := range res.Contents {
				contents = append(contents, renderResourceContent(c))
			}
			out["contents"] = contents
			return out, nil
		}); err == nil {
		out = append(out, t)
	}
	return out
}

// renderResourceContent 单条资源内容 → 模型可见形态（文本透传；blob 只报
// 类型与字节数——二进制不进模型上下文）。
func renderResourceContent(c mcp.ResourceContents) map[string]any {
	switch v := c.(type) {
	case mcp.TextResourceContents:
		return map[string]any{"uri": v.URI, "mime": v.MIMEType, "type": "text", "text": v.Text}
	case mcp.BlobResourceContents:
		return map[string]any{"uri": v.URI, "mime": v.MIMEType, "type": "binary",
			"note": fmt.Sprintf("二进制资源（base64 %d 字符）——未进入上下文", len(v.Blob))}
	default:
		return map[string]any{"type": "unknown", "note": "未识别的资源内容形态"}
	}
}

// instructionSection server instructions 的提示段形态（放置权归应用
// Instruction 拼装——本函数只产出内容）。
func instructionSection(serverName, instructions string) string {
	return "## MCP 服务器指令：" + serverName + "\n\n" + instructions
}
