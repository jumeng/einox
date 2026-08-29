# 03 · 装配：快速使用、自由裁剪与业务扩展

> 全部能力经 `engine.Options` 注入与启停——**四项必填，其余全部可选：nil 即不装配，零行为变化**。应用对基座只装配、不修改：组装根 = 构造 `Options` 交给 `NewManager`，构造期定型、运行期不可变（形态的完整界定见 [01-positioning.md](01-positioning.md)「形态对比」章）。

## 装配面（engine.Options）

| 字段 | 必填 | 说明 |
|---|---|---|
| `Providers` | ✅ | 模型解析（`func() []llm.ProviderSpec`；空清单 = 未配置模型错误面） |
| `Instruction` | ✅ | 系统提示词（`func(SessionBrief) string`——入参含 mode/model/effort/owner/sid，每轮实时注入） |
| `CheckPoints` | ✅ | 检查点存储构造（operator+sid 定位；接口仅 Get/Set） |
| `WorkspaceRoot` | ✅ | 会话工作区根（owner+sid 定位；repos/ 挂载持久、其余任务收尾清理） |
| `Tools` | | 业务工具面（`func(SessionBrief) []contract.Tool`——入参含会话身份，多租户按 owner 裁剪工具面；nil = 无业务工具。闭包每轮求值、跨会话并发，应快速返回） |
| `ProcessTools` | | 进程级通用件——应用**选择加入**的基座件（时钟/网页抓取等） |
| `SessionToolsOff` | | 排除的会话域工具族（todo/ask/plan/fs/cmd/patch；nil = 全挂零变化，未知名构造期即拒——fail-fast） |
| `ToolWrap` | | 工具包装缝（hitl 审批外层，主面与子代理面同挂——调用审计/结果脱敏/动态准入。只能收紧不能放宽：透传保留审批语义；拒绝以 `{"ok":false}` 信封回喂模型） |
| `NewModel` | | 模型构造口（缺省 `llm.NewChatModel`；测试注入 `llmtest` 假模型） |
| `ImageResolve` | | 图片引用解析（nil = 图片不可用，含图请求即错误面） |
| `SkillsDir` | | skill 物化目录（`func(SessionBrief) string`——按租户物化不同 skill 包；nil/空 = 不挂 skill middleware） |
| `Approval` | | 审批配置（写工具名单/动作名/ArgsForce——业务内容） |
| `SubAgents` | | spawn 子代理装配（nil = 不装配 spawn） |
| `Topology` | | 确定性多 agent 拓扑（nil = 单 agent react 主线） |
| `ToolSearchPolicy` | | 动态工具装载（nil = 全量常驻零变化） |
| `RepoMounts` | | 代码仓定位 Resolver（nil = 不装配 repo 族） |
| `RepoPatchWriter` | | 补丁导出落盘（nil = 导出面报未配置） |
| `Sandbox` | | run_command 沙箱策略（nil = 不沙箱） |
| `SandboxProvider` | | 沙箱后端（nil = OSProvider 平台内建；容器形态注入 `&sandbox.DockerProvider{Image: …}`——旧 `EINO_RUN_DOCKER` env 开关已退役） |
| `Egress` | | 网络出口校验器（nil = 不预检零变化） |
| `SummarizerFallbackModels` | | 摘要模型 Failover 降级链（空 = 不配降级） |

## 最小装配

```go
reg := session.NewRegistry(store) // store 实现 session.Store（落盘归应用）

m, err := engine.NewManager(reg, engine.Options{
    Providers:    func() []llm.ProviderSpec { return llm.ResolveMerged(cfg) },
    Instruction:  func(sess engine.SessionBrief) string { return myInstruction(sess) },
    Tools:        func(sess engine.SessionBrief) []contract.Tool { return myBusinessTools(sess) },
    CheckPoints:  func(operator, sid string) engine.CheckPointStore { return myStore(operator, sid) },
    WorkspaceRoot: func(owner, sid string) string { return filepath.Join(dataDir, owner, "workspaces", sid) },
})
if err != nil {
    // 装配错误启动期即拒（如 SessionToolsOff 未知名）——不拖到首会话
}

// 运行面：事件经回调扇出（SSE/WebSocket/CLI 传输归应用；事件同时落会话记录）
m.Run(ctx, sess, userMsg, attachments, func(ev session.Event) { /* 编码转发 */ })
m.Resume(ctx, sess, fn)     // 审批/提问/计划决议后续流
m.FlushQueue(sess)          // 排队消息落回轮
```

提示词拼装的惯用形态：业务职责段（你写）+ `prompts.Coding()`（工作区工具面在场时）+ `prompts.Orchestration()`（spawn 装配时）+ 会话配置段（mode 语义——manual/plan/auto）。

测试注入：`NewModel: llmtest.…`（假模型）即可零真实端点跑通引擎与工具循环，支持逐轮注错。

## 自由裁剪

裁剪 = 不注入对应 Options 字段，没有第二套开关：

| 想要 | 做法 |
|---|---|
| 不要子代理 | `SubAgents` 留 nil（提示词不拼 `Orchestration()` 段） |
| 不要沙箱 | `Sandbox` 留 nil（默认即关，opt-in） |
| 不要出口治理 | `Egress` 留 nil |
| 沙箱开 workspace-write | 见下「沙箱装配」——`WritableRoots` + 缓存重定向 `Env` 是保命件 |
| 大工具面瘦身 | `ToolSearchPolicy{DynamicTools: [...]}`——高频件与 ask_user/todo_write/submit_plan 留常驻 |
| 极简装配（纯业务问答） | `SessionToolsOff: []string{engine.FamilyFS, engine.FamilyCmd, engine.FamilyPatch}`——物理移除文件/命令/补丁面；此时 Instruction 勿拼 `prompts.Coding()` 段 |
| 确定性场景多 agent | `Topology{Kind, SubAgents}`（supervisor/deep；默认单 agent） |
| 子代理限权 | `SubAgentsConfig{Tools: 白名单, DenyTools: 硬拒名单}`——交集或含全量面未同名 = 装配期报错（白名单写漏即暴露，fail-fast） |
| 基座件按需 | `ProcessTools` 只放你要的（时钟/网页抓取各自独立构造） |

**沙箱装配**（OS 后端 = re-exec 哨兵协议——应用 main 需挂 `sandbox.RunHelper` 钩子，装配期经沙箱 Provider 探测，内核不可用启动告警；`SandboxProvider` 注入容器等后端时无哨兵依赖）：

```go
&Sandbox: &sandbox.Policy{
    Mode:          sandbox.ModeWorkspaceWrite, // readonly / workspace-write / danger-full-access
    Network:       true,   // 断网档下依赖安装必死且模型无法自纠——内网形态须配 on
    EnvMode:       sandbox.EnvMinimal,         // 环境白名单——凭据面默认不进围栏（缺省 inherit 全继承）
    WritableRoots: []string{cacheDir},          // 围栏内 HOME 不可写——缓存必须落在可写根
    Env: []string{                              // 缓存重定向（不重定向 go build 硬失败无回退）
        "GOCACHE=" + cacheDir + "/go-build",
        "GOMODCACHE=" + cacheDir + "/go-mod",
        "npm_config_cache=" + cacheDir + "/npm",
    },
}
```

容器形态：`SandboxProvider: &sandbox.DockerProvider{Image: "golang:1.26"}`——策略翻译进容器参数（三档挂载映射/断网/可写根/ro 子挂载回盖；旧 `EINO_RUN_DOCKER` env 开关已退役）。

**出口治理**：`egress.New([]string{"10.0.0.0/8", ...})`——私网默认阻断 + 白名单即工作面；建议开关打开而白名单缺失时启动即报错（fail-fast，防「开了开关忘了白名单」）。

## 扩展业务能力

### 1. 业务工具：实现 contract.Tool

```go
type createOrder struct{ dep MyDeps }

func (t *createOrder) Info() *contract.ToolInfo {
    return &contract.ToolInfo{
        Name: "create_order",
        Desc: "创建订单（批量）……",
        Params: &contract.Schema{Type: "object", Properties: map[string]*contract.Schema{
            "title": {Type: "string"},
        }, Required: []string{"title"}},
        Behavior: contract.BehaviorWrite, // 展示语义（探索/更改/终端分组）
    }
}

func (t *createOrder) Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
    // 业务性失败：{"ok":false,"error":…} 回喂模型自纠（Go error 会终止整轮）
    // 需要挂起交互（审批同构）：return nil, &contract.Suspend{…}
}
```

要点：工具实现**不落审批语义**——写面审批由基座组装期 `hitl.WrapTools` 统一包装；批量优先、确定性部分（校验/ID 生成/抽取）在工具内部完成不经 LLM。Suspend 的自定义 `Info/State` 类型须先经 `einoext.RegisterSuspendState[T]()` 注册（checkpoint gob 序列化义务——业务不 import eino 即可满足，未注册首个挂起即报错）。

### 2. 审批名单（业务内容）

`Approval: hitl.ApprovalConfig{…}` 定三件事：哪些工具算**写**（进审批矩阵）、各工具审批动作名、**ArgsForce 名单**（如「置完成」「commit」——任何模式/任务期授权不豁免，人工逐次批准）。三档会话模式（manual 逐写审批 / plan 计划卡 / auto 直过）由引擎按模式包装，名单不变。

### 3. 提示词与 skill

- `Instruction` 回调拼装你的业务职责段 + 基座通用段（`prompts.Coding()` / `Orchestration()`）+ 模式语义段；
- `SkillsDir` 指向你的 skill 物化目录即启用 skill middleware（SKILL.md 按 agentskills.io 标准自动发现；物化/分发归应用）。

### 4. 外部生态

`einoext.NewExtTools(dir, einoext.MCPSpec{URL: …, Cmd: …})` 接入 eino-ext 官方组件与 MCP 远端件（`mcp_*` 前缀；写面语义未知 fail-closed——建议按写审批）。

### 5. 子代理白名单（多代理编排的权限面）

`SubAgents.Tools` 只列读面与工作区探索/验证件；数据域写、repo 写、采集类进 `DenyTools`——子代理只读勘察，数据变更以建议清单回传、父用自有写工具代执行走既有审批。并发上限 `MaxConcurrent` 照看 LLM 端点压力（超限信号量排队不失败）。

## 业务侧边界守卫（建议）

「业务 0 import eino」是架构验收线，守卫放业务仓（检查的是 eino 这个固定名字，与业务仓自己叫什么无关）。任一业务仓在仓根放一个测试文件即可：

```go
func TestNoEinoImports(t *testing.T) {
	// root 指向业务仓根（按测试文件所在目录调整相对层级）
	cmd := exec.Command("go", "list", "-test", "-f",
		"{{.ImportPath}}\t{{range .Imports}}{{.}} {{end}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg, imports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, imp := range strings.Fields(imports) {
			if imp == "github.com/cloudwego/eino" || strings.HasPrefix(imp, "github.com/cloudwego/eino/") {
				t.Errorf("业务禁直接依赖 eino：%s → %s（只应 import 基座契约面）", pkg, imp)
			}
		}
	}
}
```

配套的 `-test` 注意点：`go list -test` 会输出 `[pkg.test]` 形式的测试变体引用，属输出格式而非真实 import，跳过以 `[` 开头的项即可。基座侧的反向边界（einox 不依赖任何业务模块）由 einox 仓自己的依赖白名单守卫承担，业务仓无需关心。
