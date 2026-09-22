package einoext

// eino-ext 官方工具面全量接入（自产品 internal/tools/einoutils.go 迁入）：
//   零依赖直接构造：sequentialthinking / httprequest(get|post|put|delete 族)
//     / wikipedia（缺省英文站，EINO_WIKIPEDIA_BASEURL 覆盖——语言属部署决策，
//     基座不预设）/ duckduckgo(免凭证)
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
	"github.com/jumeng/einox/sandbox"
	"github.com/jumeng/einox/tools"
)

// localOperator commandline.Operator 的本地实现（工作区限定：读写与命令
// cwd 全部圈进 root，路径穿越显式拒绝）。RunCommand 经 sandbox.BuildCommand
// （D2 缝隙①收口：python_execute 内层执行面与 run_command 同一围栏语义——
// sb 为 nil 时保持直执行现状，路径圈禁仍在；require 姿态后端不可用拒跑）；
// 工具调用本身照常过 hitl 审批与 ToolWrap（ProcessTools 面的标准链路）。
type localOperator struct {
	root string
	sb   *sandbox.Policy  // nil = 直执行现状（ExtConfig.OperatorSandbox 注入）
	sp   sandbox.Provider // nil = OSProvider（BuildCommand 内归一）
}

// abs 归一并校验 containment。filepath.Join 会把 ".." 清洗进结果路径——
// Join(root, "../../etc") 结果逃出 root，必须以 Rel 显式拒绝（P0 安全修复）。
// abs 圈禁 + 绝对化（圈禁判定与 fsutil/office/applypatch 同源——tools.
// ResolveUnder 单点，审查 P1-5；本地算子返回绝对路径供直读）。
func (o *localOperator) abs(p string) (string, error) {
	under, err := tools.ResolveUnder(o.root, p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(under)
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
	cmd, _, err := sandbox.BuildCommand(ctx, o.root, o.root, o.sb, o.sp, command, nil)
	if err != nil {
		// require 姿态 fail-closed：拒跑经 errFeed 语义回喂（非零退出同款
		// 形态——输出并入 Stdout，模型可见可自纠/转告用户）
		return &commandline.CommandOutput{Stdout: "[沙箱拒绝] " + err.Error()}, nil
	}
	out, err := cmd.CombinedOutput() // 命令 cwd 圈进工作区（相对路径产物落工作区）
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
// face 携握手捕获的完整面（工具 + 资源工具 + server instructions——N2）。
type mcpFace struct {
	tools        []tool.BaseTool
	resTools     []contract.Tool
	instructions string // 提示段成品（serverName+指令文本；空 = 无）
}

var (
	mcpCacheMu  sync.Mutex
	mcpCacheKey string
	mcpFaces    = map[string]*mcpFace{}
)

// MCPCacheStatus 当前缓存态（应用侧展示：连接面 + 拉到的工具名）。
func MCPCacheStatus() (key string, names []string) {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	for _, f := range mcpFaces {
		for _, t := range f.tools {
			if it, ok := t.(tool.InvokableTool); ok {
				if info, err := it.Info(context.Background()); err == nil && info != nil {
					names = append(names, info.Name)
				}
			}
		}
	}
	return mcpCacheKey, names
}

// MCPSection 当前连接的 server instructions 提示段（空 = 无——未配置/
// 未连接/服务端未给；放置权归应用 Instruction 拼装，机制与内容分离）。
func MCPSection() string {
	mcpCacheMu.Lock()
	defer mcpCacheMu.Unlock()
	if mcpCacheKey == "" {
		return ""
	}
	return mcpFaces[mcpCacheKey].instructions
}

// mcpFaceOf 取 MCP 面（缓存命中直用；新键拨号失败缓存空防反复重试）。
func mcpFaceOf(ctx context.Context, spec MCPSpec) *mcpFace {
	if spec.URL == "" && spec.Cmd == "" {
		return nil
	}
	key := spec.URL + "|" + spec.Cmd
	mcpCacheMu.Lock()
	if f, ok := mcpFaces[key]; ok {
		mcpCacheMu.Unlock()
		return f
	}
	mcpCacheMu.Unlock()

	var f *mcpFace
	if spec.URL != "" {
		f = newMCPFace(ctx, spec.URL)
	} else {
		f = newMCPStdioFace(ctx, strings.Fields(spec.Cmd))
	}
	mcpCacheMu.Lock()
	mcpCacheKey = key
	mcpFaces[key] = f
	mcpCacheMu.Unlock()
	return f
}

// envOr env 后备取值（空值视为未配置回退缺省）。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ExtConfig 可选扩展配置（零值 = NewExtTools 兼容门面的等价形态——零变化）。
type ExtConfig struct {
	// OperatorSandbox python_execute 内层执行面沙箱（D2 缝隙①收口：nil =
	// 直执行现状〔localOperator 的路径圈禁仍在〕；注入后与 run_command
	// 同一围栏语义，require 姿态后端不可用拒跑）。
	OperatorSandbox  *sandbox.Policy
	OperatorProvider sandbox.Provider // nil = sandbox.OSProvider
}

// NewExtTools 组装 eino-ext 全部工具（一个不少；失败容忍降级），经 Bridge
// 入契约面。root = commandline 工作区根（.tmp 同级临时域，惰性创建；
// EINO_OPERATOR_ROOT 显式覆盖——运维需要更大面时的显式让渡）。
// mcp = MCP 接入来源（应用配置解出；空则 env 后备）。
func NewExtTools(root string, mcp MCPSpec) []contract.Tool {
	return NewExtToolsWith(root, mcp, ExtConfig{})
}

// NewExtToolsWith 全量装配面（NewExtTools 的带配置版——python_execute
// 沙箱经 ExtConfig 注入，其余同款）。
func NewExtToolsWith(root string, mcp MCPSpec, cfg ExtConfig) []contract.Tool {
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
		// 语言站点属部署决策不进基座默认（2026-09-22 边界审查：此前硬编码
		// 中文站+产品名 UA 属产品迁移残留）；缺省英文站 = 上游组件默认。
		BaseURL:   envOr("EINO_WIKIPEDIA_BASEURL", "https://en.wikipedia.org/w/api.php"),
		UserAgent: "github.com/jumeng/einox/0.1",
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
	op := &localOperator{root: root, sb: cfg.OperatorSandbox, sp: cfg.OperatorProvider} // 根不随组装落盘，惰性建（见 RunCommand）
	if py, err := commandline.NewPyExecutor(ctx, &commandline.PyExecutorConfig{Operator: op}); err == nil {
		out = append(out, py)
	}

	// browseruse：显式开关（首次构造可能下载浏览器，阻塞启动）
	if os.Getenv("EINO_BROWSERUSE") == "1" {
		if t, err := browseruse.NewBrowserUseTool(ctx, &browseruse.Config{Headless: true}); err == nil {
			out = append(out, t)
		}
	}

	// mcp：spec（应用配置）优先，env 后备；进程级缓存防每轮重拨。资源工具
	// 与工具面同缓存态（握手捕获）；instructions 经 MCPSection() 由应用
	// Instruction 拼装（NewExtTools 不碰提示面——放置权归应用）。
	if mcp.URL == "" && mcp.Cmd == "" {
		mcp = MCPSpec{URL: os.Getenv("EINO_MCP_URL"), Cmd: os.Getenv("EINO_MCP_CMD")}
	}
	out = append(out, mcpFaceOf(ctx, mcp).tools...)
	return append(Bridge(out), mcpFaceOf(ctx, mcp).resTools...)
}

// newMCPStdioFace 启动 stdio MCP 子进程并握手（失败静默跳过）。
func newMCPStdioFace(ctx context.Context, argv []string) *mcpFace {
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

// newMCPFace 连接 MCP 服务并握手（失败静默跳过——服务不可达不阻断启动）。
func newMCPFace(ctx context.Context, url string) *mcpFace {
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

// mcpHandshake 初始化握手 + 拉取工具（mcp_ 前缀改名）+ 捕获资源面与
// server instructions（N2）。失败路径关连接——stdio 形态下客户端挂着子
// 进程，泄漏即进程永久滞留；instructions 超限同视为配置错误整个来源跳过
// （fail-closed 于提示注入面——dsh「超限连接失败」同款）。
func mcpHandshake(ctx context.Context, cli *client.Client) *mcpFace {
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "einox", Version: "0.1"}
	res, err := cli.Initialize(ctx, req)
	if err != nil {
		_ = cli.Close()
		return nil
	}
	if len(res.Instructions) > maxMCPInstructionBytes {
		_ = cli.Close()
		return nil
	}
	f := &mcpFace{}
	if res.Capabilities.Resources != nil {
		f.resTools = newResourceTools(cli, res.ServerInfo.Name)
	}
	if res.Instructions != "" {
		f.instructions = instructionSection(res.ServerInfo.Name, res.Instructions)
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
	f.tools = out
	return f
}
