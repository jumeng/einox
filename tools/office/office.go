// Package office 提供 office 文档读写工具族（2026-08-25 裁决：office 读写是
// 通用能力，归基座）：read_xlsx / write_xlsx / read_docx / write_docx /
// read_pptx。零第三方依赖——OOXML 即 zip+xml，读写全走标准库手写（对齐容器
// 离线约束与产品 read_document 纯 Go 先例）。写面 = 新建/覆盖工作区内目标
// 文件；审批由基座 hitl 组装期包装，工具内不落审批语义（与 fsutil 同纪律）。
// 全部路径圈进工作区根，穿越显式拒绝（fail-closed）。
package office

import (
	"fmt"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Config 构造配置。Root = 会话工作区根——空值拒绝构造（不给全盘默认）。
type Config struct {
	Root string
}

// NewTools 构造五件（写面审批由组装层包装）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("office 需要工作区根（拒绝全盘默认）")
	}
	h := &helper{root: cfg.Root}
	wx, err := tools.InferTool("write_xlsx",
		"在工作区创建 xlsx 工作簿（同名覆盖）。sheets 为工作表数组：name + rows（二维单元格，值支持字符串/数字/布尔，空串 = 空单元格，稀疏行列自动补位）。上限 32 表 × 5000 行 × 256 列；纯内容无样式面（无合并单元格/公式/格式），要样式请生成后人工处理。",
		h.writeXlsx)
	if err != nil {
		return nil, err
	}
	rx, err := tools.InferTool("read_xlsx",
		"读取 xlsx 工作簿单元格（字符串形态输出）。sheet 指定工作表名（空 = 第一张），sheets 返回全部表名；rows 为该表二维字符串数组（空单元格为空串，稀疏区补齐）。注意：日期单元格输出 Excel 序列数，需自行换算。上限 2000 行 × 256 列，超限截断并标记 truncated。",
		h.readXlsx)
	if err != nil {
		return nil, err
	}
	wd, err := tools.InferTool("write_docx",
		"在工作区创建 docx 文档（同名覆盖）。blocks 为内容块数组，type 取值：heading（level 1-4 标题）/ paragraph（正文段）/ list_item（列表项，以圆点段落呈现）/ table（rows 二维表格，首行加粗作表头）。纯内容无样式定制，中文字体随打开端默认。",
		h.writeDocx)
	if err != nil {
		return nil, err
	}
	rd, err := tools.InferTool("read_docx",
		"读取 docx 正文文本：段落逐行输出，表格每行输出一行、单元格以「 | 」连接。页眉页脚/脚注/文本框等非正文不抽取。",
		h.readDocx)
	if err != nil {
		return nil, err
	}
	rp, err := tools.InferTool("read_pptx",
		"读取 pptx 各幻灯片文本（slides 按幻灯片顺序，页内各文本框以换行拼接）。图形/图片/演讲者备注不抽取。",
		h.readPptx)
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(wx, contract.BehaviorWrite), tools.WithBehavior(rx, contract.BehaviorRead), tools.WithBehavior(wd, contract.BehaviorWrite), tools.WithBehavior(rd, contract.BehaviorRead), tools.WithBehavior(rp, contract.BehaviorRead)}, nil
}

// fail 统一失败输出（回喂模型自纠，errFeed 语义——与 fsutil 一致）。
func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil
}
