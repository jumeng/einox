// capability SID 派生（windows 后端的平台无关部分，真源 §4）：按可写根
// 路径确定性派生 S-1-4-x-y（dsh workspace-sid.ts 蓝本，subauthority 30-bit）。
// 确定性 = 跨会话/跨进程重启稳定——工作区根的 standing ACE 只需授予一次
// （codex cap.rs 的随机 SID 方案要持久化状态文件，丢档即孤儿 ACE）。SID
// 字符串本身不是秘密：权力完全由命名它的 ACE 定义，且只在本进程铸造的
// restricted token 里携带。
package sandbox

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// WorkspaceWriteSID 可写根 → 确定性 capability SID（S-1-4-x-y）。
func WorkspaceWriteSID(root string) string {
	sum := sha256.Sum256([]byte(canonicalPathKey(root)))
	first := binary.LittleEndian.Uint32(sum[0:4])%(1<<30-1) + 1
	second := binary.LittleEndian.Uint32(sum[4:8])%(1<<30-1) + 1
	return fmt.Sprintf("S-1-4-%d-%d", first, second)
}

// canonicalPathKey windows 路径规范化键（大小写不敏感 + 分隔符归一 + 去重
// 分隔与尾缀）：两种拼法派生同一 SID——ACL 复用不因拼写分叉出自建第二身份
// （dsh「canonicalization converges case/alias spellings」同款语义）。
// 归一强度注记（收官二轮审查 C-5）：dsh 输入是 realpath 级全解析（8.3 短名/
// junction 收敛），本键只做拼写级归一——同目录的短名/junction 拼法会铸出第
// 二身份并落第二条 ACE；dsh 自注此形态「self-healing, at the cost of one
// extra tree propagation」——自愈非破绽（两条身份授权同一棵树），代价记档
// 真源 §11.10。
func canonicalPathKey(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "/", `\`)
	for strings.Contains(p, `\\`) {
		p = strings.ReplaceAll(p, `\\`, `\`)
	}
	return strings.TrimSuffix(p, `\`)
}
