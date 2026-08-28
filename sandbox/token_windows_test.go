//go:build windows

// 掩码位级锚（收官二轮审查 A-1 防回归）：allow 掩码不得含 FILE_DELETE_CHILD
// ——父目录删子权会绕过保护子路径上的 deny ACE（删除经父权限授权时不查询
// 子对象 DACL）。本机 Application Control 拦 go test，随首次 windows 运行时
// 验证跑（含 A-1 修复的 rm -rf <protected> 现场验证）。
package sandbox

import "testing"

func TestWriteMasks(t *testing.T) {
	if writeAllowMask&fileDeleteChild != 0 {
		t.Fatal("allow 掩码不得含 FILE_DELETE_CHILD（父目录删子权绕过 deny ACE——审查 A-1）")
	}
	if writeAllowMask&deleteRight == 0 || writeAllowMask&fileGenericWrite != fileGenericWrite {
		t.Fatal("allow 掩码须含 DELETE 与完整写位（rm/mv 经对象自身 DELETE 可用）")
	}
	if writeDenyMask&fileDeleteChild == 0 || writeDenyMask&deleteRight == 0 {
		t.Fatal("deny 掩码须同时封两条删除路径（DELETE | FILE_DELETE_CHILD）")
	}
}
