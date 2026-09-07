# 渠道样板(channels)

> 清单域 ⑧:`channels`(feishu / voice / 自定义)。渠道三层:机制核 `engine/channel.go`(渠道无关编排)→ 官方通用件 `channels/` → 业务自定义渠道长在业务仓。引擎只见「文本轮次进、事件流出」。

## feishu(官方通用件,两段式装配)

```go
b := feishu.New(feishu.Config{
    AppID:     os.Getenv("FEISHU_APP_ID"),
    AppSecret: os.Getenv("FEISHU_APP_SECRET"),
    Model:     "deepseek/deepseek-chat", // 渠道起轮用的模型复合键
})

m, err := engine.NewManager(reg, engine.Options{
    Channels: []engine.ChannelConfig{
        {ID: b.ID(), Model: "deepseek/deepseek-chat", Sink: b},
    },
    // ……其余 Options
})
if err != nil { /* …… */ }

if err := b.Start(m); err != nil { // 绑定网关起长连接
    log.Fatal(err)
}
defer b.Close()
```

装配后渠道编排面懒建可用:`m.Channels()`——入站 `Handle` 分流(空闲起轮/运行中 Steer 排队/挂起接决议续流)、常驻事件订阅出站(覆盖挂起期)、`Approve`/`Answer` 决议回写、`Cancel` 停轮、`Push` 主动通知。`(channel, chat)→sid` 绑定表 UserTree 落盘,重启经 Reattach 找回。

不 import `channels/feishu` 则 SDK 依赖不进构建。`channels/voice` 是占位零实现(流型语音供应商口,转写/合成接口待首个实现定稿)。

## 自定义渠道(业务仓实现)

两件套:

```go
// ① 出站:实现 ChannelSink(渲染归适配器)
type mySink struct{ /* 协议客户端 */ }

func (s *mySink) Deliver(b engine.ChannelBrief, ev session.Event) {
    // 渲染事件为渠道消息形态并投递;覆盖挂起期与后台通知
}

// ② 入站:持有协议收发,文本轮次喂 Handle、决议调 Approve/Answer
gw := m.Channels()
gw.Handle(engine.InboundMsg{ /* Channel, Chat, Text, Speaker… 协议消息归一 */ })
gw.Approve(sid, itemID, &contract.Participant{ID: "u1"}, contract.ApprovalDecision{Approved: true})
gw.Answer(sid, &contract.Participant{ID: "u1"}, contract.AskDecision{ /* …… */ })

// 装配同 feishu:
Channels: []engine.ChannelConfig{{ID: "mine", Model: "deepseek/deepseek-chat", Sink: &mySink{}}},
```

多 agent 共用渠道 = 应用层多 Manager/多 Gateway 组合,基座不建路由器。

## 停机序

```
HTTP Shutdown → Channels().Close → reg.Drain(deadline) → store Close
```

先停渠道事件投递面,再收执行体(终态落盘依赖 store 存活);Drain 到点未收尾的 SID 如实返回(记日志不阻塞停机)。

## 验证

- `go build ./...` 过(import feishu 才进 SDK 依赖)。
- feishu 冒烟需真实应用凭据;纯装配验证可用自定义 Sink 打印事件流。
