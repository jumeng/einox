# 工具面样板(tools)

> 清单域 ④:`session-tools-off` / `process-tools` / `office` / `einoext` / `egress`。工具按装配方分三类:会话域件(引擎随会话装配)、工作区件(经 `Tools` 构造)、进程级件(经 `ProcessTools` 选择加入)。

## session-tools-off(裁剪会话域族)

```go
// 极简问答形态:物理移除文件/命令/补丁面
SessionToolsOff: []string{engine.FamilyFS, engine.FamilyCmd, engine.FamilyPatch},
// 此时 Instruction 勿拼 prompts.Coding() 段;超长工具结果只剩截断头尾
// (外置换指针经 read_file 虚拟路径取回,fs 族已裁)

// 极简编码 profile(反向取舍:裁交互三件保执行三件——单用户 CLI 形态)
SessionToolsOff: []string{engine.FamilyTodo, engine.FamilyAsk, engine.FamilyPlan},
```

合法族:`engine.FamilyTodo / FamilyAsk / FamilyPlan / FamilyFS / FamilyCmd / FamilyPatch`(未知名 NewManager 即拒)。裁 `FamilyFS` = 放弃 reduction 外置换指针取回——引擎不联动,装配者知情决策。

## process-tools(进程级件)

```go
ProcessTools: func() []contract.Tool {
    var ts []contract.Tool
    if ct, err := currenttime.NewTools(); err == nil { // 时钟:周期/deadline 计算的确定性底线
        ts = append(ts, ct...)
    }
    if wf, err := webfetch.NewTools(webfetch.Config{ // 网页抓取:URL → markdown
        MaxBytes: 2 << 20,
        Egress:   egressValidator, // 可选:出口治理注入(见下节)
    }); err == nil {
        ts = append(ts, wf...)
    }
    return ts
},
```

## office(工作区件,经 Tools 装配)

前置:workspace-root(必填恒在;路径圈进工作区根)。

```go
Tools: func(sess engine.SessionBrief) []contract.Tool {
    var ts []contract.Tool
    // 业务工具在此追加(实现 contract.Tool)……
    if ot, err := office.NewTools(office.Config{
        Root: filepath.Join(dataDir, "workspaces", sess.SID), // 与 WorkspaceRoot 同规则
    }); err == nil {
        ts = append(ts, ot...) // write_docx/read_docx/write_xlsx/read_xlsx/read_pptx
    }
    return ts
},
```

`Config{Root string}` 空值拒构造。业务工具与 office 同位——`Tools` 闭包每轮 assemble 求值、跨会话并发调用,应快速返回且无共享可变态。

## einoext(eino-ext 生态件,含 MCP)

```go
Tools: func(sess engine.SessionBrief) []contract.Tool {
    root := filepath.Join(dataDir, "workspaces", sess.SID)
    ts := einoext.NewExtTools(root, einoext.MCPSpec{
        URL: "https://mcp.example.com/sse", // SSE 远端件
        // Cmd: "npx -y some-mcp-server",   // 或 stdio 形态(二选一;空则 env 后备)
    })
    // ……
},
```

MCP 工具名前缀 `mcp_`;失败容忍降级(不可达不阻断装配)。搜索/浏览器/维基等官方件同口接入。

## egress(出口治理,校验器非工具)

```go
v, err := egress.New([]string{"10.0.0.0/8"}) // 私网默认阻断(RFC1918 等)+ CIDR 白名单即工作面
if err != nil {                              // 仅 CIDR 非法报错;白名单是否强制由应用装配层决定
    log.Fatal(err)
}

Egress: v,        // Options 命令面:run_command 命令串 URL 预检
// webfetch.Config{Egress: v} — web_fetch 前置同注(见 process-tools 节)
```

与沙箱正交的用户态出口治理;部署形态边界选择见 [security.md](security.md)。

## 验证

- `go build ./...` 过。
- 未知名族:`engine: 未知的会话域工具族 "xxx"` NewManager 即拒。
- office 空根:构造返回 error。
