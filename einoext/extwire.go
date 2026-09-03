package einoext

// eino-ext 官方工具面全量接入（自产品 internal/tools/einoutils.go 迁入）：
//   零依赖直接构造：sequentialthinking / httprequest(get|post|put|delete 族)
//     / wikipedia(中文站) / duckduckgo(免凭证)
//   env 凭证/端点，有配置即生效：bingsearch(BING_API_KEY)
//     / googlesearch(GOOGLE_API_KEY+GOOGLE_CSE_ID) / searxng(SEARXNG_URL)
//     / mcp(EINO_MCP_URL，SSE 端点，启动时握手拉取远端工具，拉取工具一律
//     改名 mcp_<name>——远端语义未知，fail-closed 按写工具进审批矩阵)
//   本地环境：commandline(PyExecutor python 执行，Operator=root 限定工作区
//     ——路径防穿越[Join 清洗 .. 可逃出 root，显式拒绝]；根收敛注入的 root
//     （.agent 同级临时域，惰性创建）；python_execute 写面进审批名单)
//   显式开关：browseruse(EINO_BROWSERUSE=1——首次可能触发浏览器下载，
//     阻塞启动故不默认)
// 产物经 Bridge 入契约面（依赖与适配归基座）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/bingsearch"
	"github.com/cloudwego/eino-ext/components/tool/browseruse"
	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/duckduckgo"
	"github.com/cloudwego/eino-ext/components/tool/googlesearch"
	"github.com/cloudwego/eino-ext/components/tool/httprequest"
	mcpclient "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/cloudwego/eino-ext/components/tool/searxng"
	"github.com/cloudwego/eino-ext/components/tool/sequentialthinking"
	"github.com/cloudwego/eino-ext/components/tool/wikipedia"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/mark3labs/mcp-go/client"
	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/jumeng/einox/contract"
)

// localOperator commandline.Operator 的本地实现（工作区限定：读写与命令
// cwd 全部圈进 root，路径穿越显式拒绝）。注意其 RunCommand 是裸 exec——
// 不经 sandbox 策略（python_execute 的内层执行面，真源 2026-08-26 沙箱设计
// 缝隙①，与 run_command 的围栏语义不同面）；工具调用本身照常过 hitl 审批
// 与 ToolWrap（ProcessTools 面的标准链路）。
type localOperator struct{ root string }

// abs 归一并校验 containment。filepath.Join 会把 ".." 清洗进结果路径——
// Join(root, "../../etc") 结果逃出 root，必须以 Rel 显式拒绝（P0 安全修复）。
func (o *localOperator) abs(p string) (string, error) {
	a, err := filepath.Abs(filepath.Join(o.root, p))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(o.root, a)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界（仅限工作区 %s 内）：%s", o.root, p)
	}
	return a, nil
}

func (o *localOperator) ReadFile(_ context.Context, path string) (string, error) {
	full, err := o.abs(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(full)
	return string(b), err
}

func (o *localOperator) WriteFile(_ context.Context, path, content string) error {
	full, err := o.abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (o *localOperator) IsDirectory(_ context.Context, path string) (bool, error) {
	full, err := o.abs(path)
	if err != nil {
		return false, err
	}
	st, err := os.Stat(full)
	if err != nil {
		return false, err
	}
	return st.IsDir(), nil
}

func (o *localOperator) Exists(_ context.Context, path string) (bool, error) {
	full, err := o.abs(path)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	return err == nil, nil
}

func (o *localOperator) RunCommand(ctx context.Context, command []string) (*commandline.CommandOutput, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("空命令")
	}
	// 惰性建根：命令 cwd 必须存在，首次执行才落盘（WriteFile 的 MkdirAll
	// 同理天然覆盖；读面对缺失根表现为不存在）
	if err := os.MkdirAll(o.root, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = o.root // 命令 cwd 圈进工作区（相对路径产物落工作区）
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // 会话取消：保持上抛，不伪装成命令失败
		}
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		// 上游 PyExecutor 对非零退出会丢弃输出、双重包装成裸「execute error:
		// exit status N」——traceback 全失，模型无从自纠。改为退出码+完整输出
		// 并入 Stdout 以 nil 错误回喂（errFeed 语义，同 run_command 的 fail()）
		return &commandline.CommandOutput{
			Stdout: fmt.Sprintf("[命令失败 退出码 %d]\n%s", code, string(out)),
		}, nil
	}
	if len(out) == 0 {
		// 上游把空输出误判为错误（execute result is empty）——成功零输出属正常
		return &commandline.CommandOutput{Stdout: "(无输出)\n"}, nil
	}
	return &commandline.CommandOutput{Stdout: string(out)}, nil
}

// mcpPrefixedTool 远端 MCP 工具改名 mcp_<name>：审批矩阵按前缀识别
// （远端工具语义未知，fail-closed 一律按写工具审批）。
type mcpPrefixedTool struct {
	tool.InvokableTool
}

func (t *mcpPrefixedTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	info, err := t.InvokableTool.Info(ctx)
	if err != nil || info == nil {
		return nil, err
	}
	cp := *info
	cp.Name = "mcp_" + info.Name
	return &cp, nil
}

// MCPSpec MCP 接入来源（url = SSE 端点 / cmd = stdio 子进程，二选一）——
// 装配层从应用配置或 env 解出（config 优先，env 后备）。
type MCPSpec struct {
	URL string
	Cmd string
}

// mcpCache 进程级缓存：每轮 Run 组装会重建工具面，MCP 握手不能跟着重拨
// （5s 超时 × 每轮 = 不可接受）。键 = url|cmd；配置变更自然换键重拨。
var (
	mcpCacheMu  sync.Mutex
	mcpCacheKey string
	mcpCacheTs  = map[string][]tool.BaseTool{}
)

// MCPCacheStatus 当前缓存态（应用侧展示：连接面 + 拉到的工具名）。
func MCPCacheStatus() (key string, names []string) {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	for _, ts := range mcpCacheTs {
		for _, t := range ts {
			if it, ok := t.(tool.InvokableTool); ok {
				if info, err := it.Info(context.Background()); err == nil && info != nil {
					names = append(names, info.Name)
				}
			}
		}
	}
	return mcpCacheKey, names
}

// mcpTools 取 MCP 工具面（缓存命中直用；新键拨号失败缓存空防反复重试）。
func mcpTools(ctx context.Context, spec MCPSpec) []tool.BaseTool {
	if spec.URL == "" && spec.Cmd == "" {
		return nil
	}
	key := spec.URL + "|" + spec.Cmd
	mcpCacheMu.Lock()
	if ts, ok := mcpCacheTs[key]; ok {
		mcpCacheMu.Unlock()
		return ts
	}
	mcpCacheMu.Unlock()

	var ts []tool.BaseTool
	if spec.URL != "" {
		ts = newMCPTools(ctx, spec.URL)
	} else {
		ts = newMCPStdioTools(ctx, strings.Fields(spec.Cmd))
	}
	mcpCacheMu.Lock()
	mcpCacheKey = key
	mcpCacheTs[key] = ts
	mcpCacheMu.Unlock()
	return ts
}

// NewExtTools 组装 eino-ext 全部工具（一个不少；失败容忍降级），经 Bridge
// 入契约面。root = commandline 工作区根（.tmp 同级临时域，惰性创建；
// EINO_OPERATOR_ROOT 显式覆盖——运维需要更大面时的显式让渡）。
// mcp = MCP 接入来源（应用配置解出；空则 env 后备）。
func NewExtTools(root string, mcp MCPSpec) []contract.Tool {
	ctx := context.Background()
	var out []tool.BaseTool

	// 零依赖直接构造
	if t, err := sequentialthinking.NewTool(); err == nil {
		out = append(out, t)
	}
	if ts, err := httprequest.NewToolKit(ctx, &httprequest.Config{}); err == nil {
		out = append(out, ts...)
	}
	if t, err := wikipedia.NewTool(ctx, &wikipedia.Config{
		BaseURL:   "https://zh.wikipedia.org/w/api.php",
		UserAgent: "github.com/jumeng/einox/0.1 (team-wiki-agent)",
	}); err == nil {
		out = append(out, t)
	}
	if t, err := duckduckgo.NewTool(ctx, &duckduckgo.Config{}); err == nil {
		out = append(out, t)
	}

	// env 凭证类：有配置即生效
	if k := os.Getenv("BING_API_KEY"); k != "" {
		if t, err := bingsearch.NewTool(ctx, &bingsearch.Config{APIKey: k}); err == nil {
			out = append(out, t)
		}
	}
	if k, c := os.Getenv("GOOGLE_API_KEY"), os.Getenv("GOOGLE_CSE_ID"); k != "" && c != "" {
		if t, err := googlesearch.NewTool(ctx, &googlesearch.Config{APIKey: k, SearchEngineID: c}); err == nil {
			out = append(out, t)
		}
	}
	if u := os.Getenv("SEARXNG_URL"); u != "" {
		if t, err := searxng.BuildSearchInvokeTool(&searxng.ClientConfig{BaseUrl: u}); err == nil {
			out = append(out, t)
		}
	}

	// commandline：python 执行（工作区限定 Operator）
	if r := os.Getenv("EINO_OPERATOR_ROOT"); r != "" {
		root = r // 显式让渡：运维指定更大可达面
	}
	if root == "" {
		root = filepath.Join(os.TempDir(), "einox-workspace") // 无数据目录兜底
	}
	root, _ = filepath.Abs(root)
	op := &localOperator{root: root} // 根不随组装落盘，惰性建（见 RunCommand）
	if py, err := commandline.NewPyExecutor(ctx, &commandline.PyExecutorConfig{Operator: op}); err == nil {
		out = append(out, py)
	}

	// browseruse：显式开关（首次构造可能下载浏览器，阻塞启动）
	if os.Getenv("EINO_BROWSERUSE") == "1" {
		if t, err := browseruse.NewBrowserUseTool(ctx, &browseruse.Config{Headless: true}); err == nil {
			out = append(out, t)
		}
	}

	// mcp：spec（应用配置）优先，env 后备；进程级缓存防每轮重拨。
	if mcp.URL == "" && mcp.Cmd == "" {
		mcp = MCPSpec{URL: os.Getenv("EINO_MCP_URL"), Cmd: os.Getenv("EINO_MCP_CMD")}
	}
	out = append(out, mcpTools(ctx, mcp)...)
	return Bridge(out)
}

// newMCPStdioTools 启动 stdio MCP 子进程并拉取工具清单（失败静默跳过）。
func newMCPStdioTools(ctx context.Context, argv []string) []tool.BaseTool {
	if len(argv) == 0 {
		return nil
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cli, err := client.NewStdioMCPClient(argv[0], nil, argv[1:]...)
	if err != nil {
		return nil
	}
	if err := cli.Start(dialCtx); err != nil {
		return nil
	}
	return mcpHandshake(dialCtx, cli)
}

// newMCPTools 连接 MCP 服务并拉取其工具清单（失败静默跳过——服务不可达
// 不阻断启动）。
func newMCPTools(ctx context.Context, url string) []tool.BaseTool {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cli, err := client.NewSSEMCPClient(url)
	if err != nil {
		return nil
	}
	if err := cli.Start(dialCtx); err != nil {
		return nil
	}
	return mcpHandshake(dialCtx, cli)
}

// mcpHandshake 初始化握手 + 拉取工具 + mcp_ 前缀改名（SSE/stdio 共用）。
// 失败路径关连接——stdio 形态下客户端挂着子进程，泄漏即进程永久滞留。
func mcpHandshake(ctx context.Context, cli *client.Client) []tool.BaseTool {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "einox", Version: "0.1"}
	if _, err := cli.Initialize(ctx, req); err != nil {
		_ = cli.Close()
		return nil
	}
	ts, err := mcpclient.GetTools(ctx, &mcpclient.Config{Cli: cli})
	if err != nil {
		_ = cli.Close()
		return nil
	}
	out := make([]tool.BaseTool, 0, len(ts))
	for _, t := range ts {
		if it, ok := t.(tool.InvokableTool); ok {
			out = append(out, &mcpPrefixedTool{InvokableTool: it})
			continue
		}
		out = append(out, t)
	}
	return out
}
