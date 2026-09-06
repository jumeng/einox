package tstore

// operator 路径围栏回归（安全审查 2026-09-06）：穿越形态（分隔符/../..）
// 落隔离名——读写删皆不出 users/ 树。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorFence(t *testing.T) {
	st := New(t.TempDir())
	// 合法 owner（含 Unicode 标识）正常读写
	if err := st.WriteUserTreeFile("张三", "sessions/s1/session.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ReadUserTreeFile("张三", "sessions/s1/session.json"); !ok {
		t.Fatal("合法 owner 应可读写")
	}
	// 穿越形态：读不到合法树、写落隔离点（不落合法 owner 名下）
	if _, ok := st.ReadUserTreeFile("../张三", "sessions/s1/session.json"); ok {
		t.Fatal("穿越形态不应读到合法 owner 的树")
	}
	if err := st.WriteUserTreeFile("../张三", "x", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ReadUserTreeFile("张三", "x"); ok {
		t.Fatal("穿越形态的写不应落入合法 owner 树")
	}
	// UserTreeDir 同围栏：穿越形态的目录仍在 users/ 之下
	if dir := st.UserTreeDir("../../elsewhere"); !strings.Contains(dir, filepath.Join(st.Dir(), "users")) {
		t.Fatalf("UserTreeDir 应圈在 users/ 内：%s", dir)
	}
	// 数据根的上级不出现任何穿越产物（隔离点在树内）
	if _, err := os.Stat(filepath.Join(st.Dir(), "elsewhere")); !os.IsNotExist(err) {
		t.Fatal("穿越形态不应在数据根外产生任何落点")
	}
}
