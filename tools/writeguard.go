package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidTopDir 顶层子目录名围栏（工作区相对、单段）：非空、非 . / ..、不含
// 路径分隔符与 ..。WorkspaceKeep/WorkspaceProtect（engine.Options）与
// fsutil/applypatch 的 ProtectDirs 共用——持久区与写区一律顶层粒度，嵌套
// 子树留待真实需求再议（YAGNI）。
func ValidTopDir(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// CheckTopDirs 清单条目逐一过 ValidTopDir；what = 配置字段名（错误文案定位
// 用）。构造期 fail-fast——配置意图非法即拒，不拖到运行时（对齐
// SessionToolsOff / DenyTools 纪律）。
func CheckTopDirs(what string, dirs []string) error {
	for _, d := range dirs {
		if !ValidTopDir(d) {
			return fmt.Errorf("%s 含非法条目 %q（须为工作区内顶层目录名：非空、不含路径分隔符、非 . / ..）", what, d)
		}
	}
	return nil
}

// PathBlocked 写区命中判定：rel = 工作区内相对路径，dirs = 顶层子目录名清单
// （调用方已过 CheckTopDirs）。命中 = 目标为保护区本身或其子路径；rel 归一
// 为 "."（工作区根）时非空清单即命中——根是所有顶层区的祖先。归一覆盖 ./
// 前缀与平台分隔符（Clean + ToSlash；Linux 下反斜杠是合法文件名字符、不视作
// 分隔符——与 fsutil/applypatch 既有路径解析语义一致，Windows 模型输出的
// 反斜杠形态在 Windows 宿主上命中）。
func PathBlocked(rel string, dirs []string) bool {
	if len(dirs) == 0 {
		return false
	}
	p := filepath.ToSlash(filepath.Clean(strings.TrimSpace(rel)))
	if p == "." {
		return true
	}
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}
