# 审批域样板(hitl)

> 清单域 ③:`mode` / `args-force` / `router` / `decision-guard`。挂起-续流通道:审批卡、结构化提问、计划卡共用同一机制(引擎 Interrupt×Resume)。无决议 fail-closed 一律拒绝。

## mode 三档(会话级)

模式是**会话级**参数(`Registry.Create(owner, task, mode, prefs)`),清单 `hitl.mode` 即应用建议的缺省档:

```go
s := reg.Create("local", msg, "manual", contract.UserPrefs{Model: "deepseek/deepseek-chat"})
//                                       ^^^^^^ manual | plan | auto
```

| 档 | 语义 |
|---|---|
| manual | 写工具逐次挂起审批 |
| plan | 计划卡批准 = 任务期写授权(一次批全轮) |
| auto | 直过零挂起 |

决议入口(应用按事件流 `approval_request` 卡渲染,决议后调):`m.Resume(ctx, s, fn)` 续流;渠道形态经 `m.Channels().Approve/Answer`(见 [channels.md](channels.md))。

## 名单与参数级强制(ApprovalConfig)

```go
Approval: hitl.ApprovalConfig{
    // 写工具名单:命中的工具按模式走审批
    WriteTools: map[string]bool{"run_command": true},
    // 前缀名单:mcp_* 等远端件按前缀整族纳管
    WritePrefix: []string{"mcp_"},
    // ArgsForce:参数级强制审批——任何模式/任务期授权不豁免
    ArgsForce: map[string]func(args string) bool{
        "run_command": func(args string) bool {
            return strings.Contains(args, "rm -rf") // 高危参数形态强制审批
        },
    },
},
```

字段速查:`WriteTools` / `WritePrefix` / `ArgsForce` / `ArgsForceBy`(按说话人变体)/ `ArgsSkip` / `ForceNotes`(强制原因提示)/ `Actions`(动作名直白话)。**名单是业务内容**——清单 `args-force` 只承载工具名列表,判定逻辑归业务代码。

## router / decision-guard(T6 多参与者,可选)

```go
// ApprovalRouter:挂起卡生成时裁决「问谁」(nil = 不路由——全员可见谁先点谁决议)
ApprovalRouter: func(b engine.SessionBrief, req contract.ApprovalReq) *contract.Participant {
    // 判据归应用:按会话概要与卡面路由,如高危操作问 owner
    if b.Owner != "" {
        return &contract.Participant{ID: b.Owner, Name: b.Owner}
    }
    return nil
},

// DecisionGuard:决议带决议者时校验一致性,mismatch fail-closed 拒绝(防越权点批)
DecisionGuard: func(targetID, deciderID string) error {
    if targetID != deciderID {
        return fmt.Errorf("越权决议:目标是 %s、实际决议者 %s", targetID, deciderID)
    }
    return nil
},
```

配套:多参与者场景下说话人身份经 `m.Dispatch(s, actor, text, atts, mode)` 的 actor 携带([ui.md](ui.md) / [channels.md](channels.md) 同源编排)。

## plan 档决议链路(应用侧 API)

plan 卡挂起后的批准/拒绝链路(manual 档审批走 `Channels().Approve` 或同款 session API;plan 档专用回执):

```go
// Run 同步返回时状态 = StatePendingApproval、PendingDueOf() = "plan"
planID := s.PendingAppID()
d := contract.ApprovalDecision{Approve: true}

s.SetDecision(d)                 // 决议入槽(Resume 时 plan 工具消费——批准 = 授任务期写)
s.RecordPlanDecision(planID, d)  // 回执落流(回放重建卡片终态的真源;≠ approval 的 RecordDecision)
reg.Persist(s)
m.Resume(ctx, s, fn)             // 续流:file_ticket 等写操作在任务期授权内直落
```

要点:批准后任务期内写工具不再逐项挂起(事件流全程无 `approval_request`);挂起 + Resume 收尾的首轮照常触发异步标题生成(首轮标记锚定 Run 入口——llmtest 剧本记得留标题槽位,genTitle 走 Generate 同耗剧本)。

## 验证

- `go build ./...` 过。
- manual 档冒烟:llmtest 剧本带工具调用 → 事件流出 `approval_request`;不决议等超时 → fail-closed 拒绝信封回喂。
