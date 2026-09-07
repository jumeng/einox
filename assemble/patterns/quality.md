# 质量域样板(quality)

> 清单域 ⑥:`final-gate` / `summarizer-fallback`。

## final-gate(收束质量门)

三层约束(事前审批/事中 ErrFeed)的收束空位:自然收束后按判据强制验证,失败经 `harness_note` 门卡 + 反馈消息入史回灌重跑(有界),耗尽 error 收束不静默放行。**判据归应用**(build/test 命令或自包对抗审查),基座只持门循环机制。

```go
FinalGate: func(sess engine.SessionBrief) *engine.GateConfig {
    // 按模式/任务形态决定开门与否(闭包实时裁决)
    return &engine.GateConfig{
        Checkers: []engine.GateChecker{
            func(ctx context.Context, wsRoot string) error { // 判据在会话工作区上跑
                cmd := exec.CommandContext(ctx, "go", "build", "./...")
                cmd.Dir = wsRoot
                if out, err := cmd.CombinedOutput(); err != nil {
                    return fmt.Errorf("build 失败:\n%s", out)
                }
                return nil
            },
        },
        MaxRetries: 2, // 负数 = 缺省 2;0 = 零回灌首验即报错
    }
},
```

`GateChecker = func(ctx context.Context, workspaceRoot string) error`。前置:cmd 族在场(`final-gate: build-test` 依赖律)。挂起/中断/错误轮不触发;checker panic fail-closed。

## summarizer-fallback(摘要模型降级链)

```go
SummarizerFallbackModels: []string{"deepseek/deepseek-chat"}, // 空 = 不配零变化
```

主摘要模型失败按序降级(逐个同链包装 vision/shape 后填 adk Failover);链尽走既有清窗兜底不外抛。单端点装配配了也白配(无档可降)。

## 验证

- `go build ./...` 过。
- 门回灌冒烟:llmtest 剧本首轮产物令 checker 失败 → 事件流出 `harness_note`(Kind: gate)+ 反馈入史;耗尽 MaxRetries → error 收束。
