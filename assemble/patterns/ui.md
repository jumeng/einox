# 界面样板(ui)

> 清单域 ⑨:`ui.web` / `ui.tui`。官方通用界面件(装配先例同 channels/:应用 import 才进构建,零引擎改动)。生产暴露必配鉴权缝。

## web(REST + SSE 回放交互面)

```go
// 便捷档:构造 + 阻塞监听(独立端口跑界面)
go ui.Serve(m, ui.Config{}, ":7788")

// 或自挂 mux:
h, err := ui.New(m, ui.Config{
    // Authorize 鉴权缝(nil = 不校验——含控制端点全开放,生产暴露必配!)
    Authorize: func(r *http.Request, owner, sid string) error {
        // 校验请求身份对 owner/sid 的可见性(多租户/会话可见性归应用)
        return nil
    },
})
if err != nil { /* …… */ }
mux.Handle("/", h)
```

**Authorize nil = 含控制端点全开放**——本地开发可,生产暴露必配鉴权缝。owner 从注册表解析非 URL 参数,不可伪造。

## tui(终端回放浏览器)

```go
if err := ui.TUI(m, sid); err != nil { // 进程内零协议层
    log.Fatal(err)
}
```

单二进制终端形态(SSH 运维节点、原生架构);单会话面寻址内存注册表。

## 消息分流(ui / 渠道共用同源编排)

```go
// 空闲起轮(actor 无条件设当轮说话人)/运行中转排队署名/首见名册登记
queued, err := m.Dispatch(s, &contract.Participant{ID: "u1", Name: "张三"}, text, atts, "manual")
```

ui run 端点与 `ChannelGateway.Handle` 同源编排——返回 queued = 转排队。多参与者身份(T6 speaker/requester/decider)经 Dispatch 的 actor 携带。

## 验证

- `go build ./...` 过。
- web 冒烟:`ui.Serve` 起后 `curl :7788/api/sessions` 出列表 JSON。
