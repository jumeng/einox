package llm

// 模型调用超时层（网络容错 ①——静默死链的确定性转化）。
//
// 全链路此前零超时：组件不设 HTTP 超时、引擎 ctx 只 cancel 无 deadline，
// 网络静默死链（无 RST）下 Recv() 永久阻塞 = 会话永远 running。本层在
// 模型工厂出口统一包装（NewChatModel 内——主会话/spawn 子/拓扑子/摘要链/
// genTitle 全覆盖；llmtest 假模型走注入不经工厂，测试零影响）：
//
//   Stream    → 空闲超时（watchdog）：两 chunk 间静默超阈值即断——非整请求
//     超时，长思考流不误杀；eino 全家无 idle 机制（08-28 实查），自研。
//   Generate  → ctx deadline（单次总超时；genTitle 自有 15s 外层不受影响）。
//
// Stream 实现约束（eino schema.stream 语义：Send/Close 非并发安全，必须
// 单写者）——三体结构：
//   看门狗：空闲超阈 → close(idleFired) + 取消 wctx（生产路径解锁 HTTP 读）
//   泵：inner 唯一消费者 → items 通道（EOF = 关闭；错误原样上送）
//   复用：sw 唯一写者，select { items, idleFired }——空闲哨兵**直达**，
//     不依赖泵回报（服务器首跑实证：ctx 无视型内层会令泵永卡 inner.Recv，
//     「永不挂起」的活性必须由本层自保，不能赌内层配合）
//
// 旋钮（基座默认 + env 运维覆盖，沿 engine EINO_MAX_ITERATIONS 先例）：
//   EINO_LLM_IDLE_TIMEOUT      秒，默认 120（chunk 间静默阈值）
//   EINO_LLM_GENERATE_TIMEOUT  秒，默认 300（Generate 总超时）
//   EINO_LLM_RETRIES           次数，默认 3（engine 重试层读取）
//
// 超时产出的错误（ErrIdleTimeout / DeadlineExceeded）经 classify.go 归为
// 可重试传输类 → adk 重试层接手（网络容错 ②）。

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ErrIdleTimeout 流式空闲超时哨兵（分类器识别为可重试传输类）。
var ErrIdleTimeout = errors.New("模型连接空闲超时（长时间无数据到达）")

var (
	idleTimeout     = 120 * time.Second
	generateTimeout = 300 * time.Second

	// MaxRetries 模型调用重试上限（engine 重试层装配用；0 = 不重试）。
	MaxRetries = 3
)

func init() {
	if v, err := strconv.Atoi(os.Getenv("EINO_LLM_IDLE_TIMEOUT")); err == nil && v > 0 {
		idleTimeout = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("EINO_LLM_GENERATE_TIMEOUT")); err == nil && v > 0 {
		generateTimeout = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("EINO_LLM_RETRIES")); err == nil && v >= 0 {
		MaxRetries = v
	}
}

// NewTimeoutModel 超时包装（工厂出口统一挂接——应用不感知）。
func NewTimeoutModel(cm model.BaseModel[*schema.Message]) model.BaseModel[*schema.Message] {
	return &timeoutModel{inner: cm}
}

type timeoutModel struct {
	inner model.BaseModel[*schema.Message]
}

func (t *timeoutModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if generateTimeout <= 0 {
		return t.inner.Generate(ctx, in, opts...)
	}
	gctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()
	return t.inner.Generate(gctx, in, opts...)
}

// timeoutItem 泵 → 复用的流元素（chunk 与 err 二选一；EOF 不走元素——关通道表达）。
type timeoutItem struct {
	chunk *schema.Message
	err   error
}

func (t *timeoutModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if idleTimeout <= 0 {
		return t.inner.Stream(ctx, in, opts...)
	}
	// wctx = 外层 ctx + 看门狗可触发的取消：泵阻塞在 inner（HTTP 读）时，
	// 取消请求上下文是生产路径的唯一解锁手段（真实 SDK 均响应；哨兵投递
	// 本身不依赖它——复用的 idleFired 分支直达）。
	wctx, wcancel := context.WithCancel(ctx)
	var last atomic.Int64 // 最近活动时刻（unix nano）
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()

	sr, sw := schema.Pipe[*schema.Message](1)
	items := make(chan timeoutItem)  // 泵 → 复用
	idleFired := make(chan struct{}) // 看门狗信号（close 一次）
	stop := make(chan struct{})      // 复用退出 → 泵停推
	sendItem := func(it timeoutItem) {
		select {
		case items <- it:
		case <-stop: // 复用已退（哨兵/收线/消费侧关闭）：弃推
		}
	}

	// 看门狗：周期巡检空闲；触发即发信号 + 取消 wctx（不写管道——单写者纪律）。
	go func() {
		tk := time.NewTicker(idleTimeout / 4)
		defer tk.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-tk.C:
				if time.Since(time.Unix(0, last.Load())) >= idleTimeout {
					close(idleFired)
					wcancel()
					return
				}
			}
		}
	}()

	// 泵：inner 唯一消费者 → items（EOF 关通道；错误原样上送，空闲归因归复用）。
	go func() {
		defer close(items)
		defer wcancel()
		inner, err := t.inner.Stream(wctx, in, opts...)
		if err != nil {
			sendItem(timeoutItem{err: err})
			return
		}
		defer inner.Close()
		touch()
		for {
			chunk, err := inner.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return // 正常收尾：关 items → 复用 Close 令消费侧收 io.EOF
				}
				sendItem(timeoutItem{err: err})
				return
			}
			touch()
			sendItem(timeoutItem{chunk: chunk})
		}
	}()

	// 复用（sw 唯一写者）：转发 / 空闲哨兵直达 / 空闲取消引发的错误归一
	// 哨兵（外层取消原样透传——引擎 ABORTED 语义依赖；外层取消的活性归
	// eino 取消监控层，本层不越权）。
	go func() {
		defer sw.Close()
		defer close(stop)
		defer wcancel()
		fired := func() bool {
			select {
			case <-idleFired:
				return true
			default:
				return false
			}
		}
		for {
			select {
			case it, ok := <-items:
				if !ok {
					return
				}
				if it.err != nil && fired() && ctx.Err() == nil {
					it.err = ErrIdleTimeout // 空闲触发的取消类错误统一哨兵形态
				}
				if sw.Send(it.chunk, it.err) {
					return // 消费侧已关闭
				}
				if it.err != nil {
					return
				}
			case <-idleFired:
				sw.Send(nil, ErrIdleTimeout) // 哨兵直达：不依赖泵回报（活性自保）
				return
			}
		}
	}()
	return sr, nil
}
