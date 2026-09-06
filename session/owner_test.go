package session

// ValidOwner 结构围栏表测（安全审查 2026-09-06：?owner=../.. 直达存储层
// users/<op>/ 路径构造的穿越面——ui/渠道入口与 tstore 围栏共用的单点规则）。

import "testing"

func TestValidOwner(t *testing.T) {
	ok := []string{"张三", "u_1", "user.example", "a", ".channel-bindings", "用户-01"}
	for _, o := range ok {
		if !ValidOwner(o) {
			t.Errorf("合法标识符 %q 应通过", o)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/b", `a\b`, "..\\..", "a\x00b", "/abs", "users/../../etc"}
	for _, o := range bad {
		if ValidOwner(o) {
			t.Errorf("路径形态 %q 应被拒", o)
		}
	}
}
