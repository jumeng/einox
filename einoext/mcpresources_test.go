package einoext

// N2 MCP resources 面回归：三工具行为（list/templates/read——按需取、blob
// 不进上下文、错误信封）、指令段形态、MCPSection 空态零成本。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"
)

// fakeResourceClient 资源面假 client。
type fakeResourceClient struct {
	resources  []mcp.Resource
	templates  []mcp.ResourceTemplate
	contents   []mcp.ResourceContents
	listErr    error
	readURI    string
	listCalled int
}

func (f *fakeResourceClient) ListResources(context.Context, mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	f.listCalled++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &mcp.ListResourcesResult{Resources: f.resources}, nil
}

func (f *fakeResourceClient) ListResourceTemplates(context.Context, mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	return &mcp.ListResourceTemplatesResult{ResourceTemplates: f.templates}, nil
}

func (f *fakeResourceClient) ReadResource(_ context.Context, req mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	f.readURI = string(req.Params.URI)
	return &mcp.ReadResourceResult{Contents: f.contents}, nil
}

// invokeOf 按名取工具并调用（args 原样 JSON）。
func invokeOf(t *testing.T, name string, args string, fc *fakeResourceClient) map[string]any {
	t.Helper()
	var found bool
	var out map[string]any
	for _, tl := range newResourceTools(fc, "srv-demo") {
		if tl.Info().Name != name {
			continue
		}
		found = true
		raw, err := tl.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s Invoke 错误：%v", name, err)
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s 返回非 JSON：%v", name, err)
		}
	}
	if !found {
		t.Fatalf("工具 %s 不在资源面", name)
	}
	return out
}

func TestResourceToolsListAndRead(t *testing.T) {
	fc := &fakeResourceClient{
		resources: []mcp.Resource{{Name: "配置", URI: "cfg://app", MIMEType: "text/plain", Description: "应用配置"}},
		contents: []mcp.ResourceContents{
			mcp.TextResourceContents{URI: "cfg://app", MIMEType: "text/plain", Text: "hello"},
			mcp.BlobResourceContents{URI: "img://x", MIMEType: "image/png", Blob: strings.Repeat("A", 100)},
		},
	}
	out := invokeOf(t, "list_mcp_resources", `{}`, fc)
	if out["ok"] != true {
		t.Fatalf("列举应成功：%v", out)
	}
	res := out["resources"].([]any)[0].(map[string]any)
	if res["name"] != "配置" || res["uri"] != "cfg://app" {
		t.Fatalf("资源字段失真：%v", res)
	}

	out = invokeOf(t, "read_mcp_resource", `{"uri":"cfg://app"}`, fc)
	if fc.readURI != "cfg://app" {
		t.Fatalf("读取应透传 uri：%s", fc.readURI)
	}
	cs := out["contents"].([]any)
	txt := cs[0].(map[string]any)
	if txt["type"] != "text" || txt["text"] != "hello" {
		t.Fatalf("文本内容应透传：%v", txt)
	}
	bin := cs[1].(map[string]any)
	if bin["type"] != "binary" || strings.Contains(fmt.Sprint(bin), "AAAA") {
		t.Fatalf("二进制只报形态不进上下文：%v", bin)
	}

	out = invokeOf(t, "list_mcp_resource_templates", `{}`, fc)
	if out["ok"] != true {
		t.Fatalf("模板列举应成功：%v", out)
	}
}

func TestResourceToolsEmptyURIError(t *testing.T) {
	fc := &fakeResourceClient{}
	out := invokeOf(t, "read_mcp_resource", `{"uri":""}`, fc)
	if out["ok"] != false || !strings.Contains(out["error"].(string), "uri") {
		t.Fatalf("空 uri 应信封报错：%v", out)
	}
}

func TestResourceToolsServerErrorEnvelope(t *testing.T) {
	fc := &fakeResourceClient{listErr: context.DeadlineExceeded}
	out := invokeOf(t, "list_mcp_resources", `{}`, fc)
	if out["ok"] != false || !strings.Contains(out["error"].(string), "列举失败") {
		t.Fatalf("服务端错误应信封回喂：%v", out)
	}
}

func TestInstructionSection(t *testing.T) {
	s := instructionSection("srv-demo", "先列后读。")
	if !strings.HasPrefix(s, "## MCP 服务器指令：srv-demo") || !strings.Contains(s, "先列后读。") {
		t.Fatalf("指令段形态失真：%s", s)
	}
}

func TestMCPSectionEmptyCache(t *testing.T) {
	mcpCacheMu.Lock()
	oldKey, oldFaces := mcpCacheKey, mcpFaces
	mcpCacheKey, mcpFaces = "", map[string]*mcpFace{}
	mcpCacheMu.Unlock()
	t.Cleanup(func() {
		mcpCacheMu.Lock()
		mcpCacheKey, mcpFaces = oldKey, oldFaces
		mcpCacheMu.Unlock()
	})
	if s := MCPSection(); s != "" {
		t.Fatalf("未配置应为空段零成本：%q", s)
	}
}

func TestMCPSectionFromCache(t *testing.T) {
	mcpCacheMu.Lock()
	oldKey, oldFaces := mcpCacheKey, mcpFaces
	mcpCacheKey, mcpFaces = "k", map[string]*mcpFace{"k": {instructions: "## 段"}}
	mcpCacheMu.Unlock()
	t.Cleanup(func() {
		mcpCacheMu.Lock()
		mcpCacheKey, mcpFaces = oldKey, oldFaces
		mcpCacheMu.Unlock()
	})
	if s := MCPSection(); s != "## 段" {
		t.Fatalf("应取当前 face 指令段：%q", s)
	}
}
