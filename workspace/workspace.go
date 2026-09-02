// Package workspace 是会话工作区模型（用户域）：一轮任务一清的临时
// 落盘面——spill 溢出、编码循环中间产物；持久子区（顶层目录，如仓挂载、
// 参考资料区）由装配层经 Wipe 的 keep 参数声明豁免。根由应用经
// engine.Options 注入（产品形态 .agent/users/<op>/workspaces/<sid>/）；
// 生命周期主锚点 = 任务正常收尾即清（挂起/异常态保留待续），兜底链 = 会话
// 删除/过期/启动孤儿清扫（归 einox/session 的 Sweep，整目录移除不经
// Wipe）。2026-08-25 自 engine 折叠处立包，对齐基座落地方案的目标布局。
package workspace

import (
	"os"
	"path/filepath"
	"slices"
)

// Of 会话工作区根（惰性创建——首次落盘才建目录，不随组装空建）。
func Of(root string) string {
	_ = os.MkdirAll(root, 0o755)
	return root
}

// Wipe 任务收尾清工作区（一轮任务一清）：整删 root 下除 keep 外的全部条目。
// keep = 持久子目录名（工作区相对、顶层单段——挂载区/参考资料区等由装配
// 层声明，基座不预设名字；跨任务保留，随会话删除/过期/孤儿清扫整清）。
// 空壳根与祖先顺手回收（Remove 仅空目录可删，非空自败——keep 尚存或他组
// 会话占用时无损害）。
func Wipe(root string, keep ...string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if slices.Contains(keep, e.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	_ = os.Remove(root)                             // 空（无持久子区）才成
	_ = os.Remove(filepath.Dir(root))               // workspaces/
	_ = os.Remove(filepath.Dir(filepath.Dir(root))) // users/<操作人>/
}
