// Package skills 是 skill 挂载机制件：把物化好的 skill 目录经只读 OS
// backend 接入官方 skill middleware（注册 skill 发现工具、注入使用指令）。
//
// 装配面：引擎组装期调用 NewMiddleware（每 Run 一次）——目录缺失/空返回
// nil 不挂，纯工具对话零变化。
// 扩展点：内容源 → 磁盘的物化归应用——基座只持机制，内容归业务。
// 已知限制：backend 限定只读 OS 文件系统；注入语义（发现工具措辞、指令位
// 置）全归上游 eino skill middleware，基座不改写。
package skills

import (
	"context"
	"os"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/skill"
)

// NewMiddleware 组装期构造（每 Run 一次——backend List 读三文件，开销可忽略）。
// 物化目录缺失/空 → nil（不挂，纯工具对话）。
func NewMiddleware(ctx context.Context, baseDir string) adk.ChatModelAgentMiddleware {
	if entries, err := os.ReadDir(baseDir); err != nil || len(entries) == 0 {
		return nil
	}
	backend, err := skill.NewBackendFromFilesystem(ctx, &skill.BackendFromFilesystemConfig{
		Backend: osBackend{}, BaseDir: baseDir,
	})
	if err != nil {
		return nil
	}
	mw, err := skill.NewMiddleware(ctx, &skill.Config{Backend: backend})
	if err != nil {
		return nil
	}
	return mw
}
