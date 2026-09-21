package llm

// systemShape system 消息边界归一（N1-b，请求边界视图变换——与 historyShape
// 同域）：InHistorySystem 未声明的模型收到 mid-history system（能力模型期
// 追加的提示变更残留：模型切换/failover 降级/装配误声明）时，把最后一条
// system 归位 position-0、其余剔除——「最新生效」语义在非能力面的归一化。
// 无 mid-system 原样透传（快路径：零拷贝）；消息对象不改写（仅重排剔除），
// 存储保真与 sanitizeHistory/historyShape 同纪律。

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// NewSystemShapeModel system 消息归一包装（inHistory = ModelSpec.InHistorySystem
// ——能力位透传零变换；挂 newShapedModel 包装链，五面同源）。
func NewSystemShapeModel(inner model.BaseModel[*schema.Message], inHistory bool) model.BaseModel[*schema.Message] {
	return &systemShapeModel{inner: inner, inHistory: inHistory}
}

type systemShapeModel struct {
	inner     model.BaseModel[*schema.Message]
	inHistory bool
}

func (h *systemShapeModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return h.inner.Generate(ctx, h.shape(input), opts...)
}

func (h *systemShapeModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return h.inner.Stream(ctx, h.shape(input), opts...)
}

// shape 视图变换（能力位或无 mid-system = 原样；否则末位 system 归位）。
func (h *systemShapeModel) shape(input []*schema.Message) []*schema.Message {
	if h.inHistory {
		return input
	}
	return hoistLatestSystem(input)
}

// hoistLatestSystem mid-system 归一：最后一条 system 置于首位、其余 system
// 剔除、余消息保序；无 mid-system（无 system 或仅在首位）返回原切片零拷贝。
func hoistLatestSystem(msgs []*schema.Message) []*schema.Message {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == schema.System {
			last = i
			break
		}
	}
	if last <= 0 {
		return msgs
	}
	out := make([]*schema.Message, 0, len(msgs))
	out = append(out, msgs[last])
	for i, m := range msgs {
		if i != last && m.Role != schema.System {
			out = append(out, m)
		}
	}
	return out
}
