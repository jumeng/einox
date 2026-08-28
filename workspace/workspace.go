// Package workspace 是会话工作区模型（用户域）：一轮任务一清的临时
// 落盘面——spill 溢出、编码循环中间产物（2026-08-25 起 repos/ 子目录为
// 会话级持久挂载区，wipe 排除）。根由应用经 engine.Options 注入
// （产品形态 .agent/users/<op>/workspaces/<sid>/）；生命周期主锚点 = 任务正常
// 收尾即清（挂起/异常态保留待续），兜底链 = 会话删除/过期/启动孤儿清扫
// （归 einox/session 的 Sweep）。2026-08-25 自 engine 折叠处立包，对齐
// 基座落地方案的目标布局。
package workspace

import (
	"os"
	"path/filepath"
)

// Of 会话工作区根（惰性创建——首次落盘才建目录，不随组装空建）。
func Of(root string) string {
	_ = os.MkdirAll(root, 0o755)
	return root
}

// Wipe 任务收尾清工作区（一轮任务一清）：整删 root 下除 repos/ 外的全部条目
// ——repos/ 是代码仓 worktree 挂载区（会话级持久：跨任务保留未提交改动与
// 分支，随会话删除整清，见 session.Registry.Delete）。spill 指针与中间产物
// 照旧跨轮不保留（交付物须落正式数据域，提示词已约定）。空壳根与祖先顺手
// 回收（Remove 仅空目录可删，非空自败——repos/ 尚存或他组会话占用时无损害）。
func Wipe(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "repos" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	_ = os.Remove(root)                             // 空（无 repos/ 挂载）才成
	_ = os.Remove(filepath.Dir(root))               // workspaces/
	_ = os.Remove(filepath.Dir(filepath.Dir(root))) // users/<操作人>/
}
