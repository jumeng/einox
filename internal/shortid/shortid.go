// Package shortid 短标识生成（前缀 + crypto/rand hex）——仓内各域 ID 的
// 单点实现。2026-09-06 收口（审查 P3-2）：rand-hex 生成曾六处各写（engine
// 挂起域三份、session 会话/队列两份、hitl 项一份），前缀语义表分散在多包
// 注释里。前缀分配表（同前缀不同域已核实无碰撞面——如 q 在 engine=提问、
// session=队列，两者不在同一名空间）：
//
//	a = 审批（engine）  q = 提问（engine）/ 排队消息（session）  p = 计划（engine）
//	s = 会话（session）  i = hitl 决议项
package shortid

import (
	"crypto/rand"
	"encoding/hex"
)

// Hex 前缀 + n 字节随机数的 hex 编码（2n 字符）。
func Hex(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
