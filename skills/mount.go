package skills

// skill 挂载（自产品 internal/agent/skills.go 的组装段迁入）：物化目录 +
// 只读 OS backend → 官方 skill middleware（注册 skill 发现工具注入指令）。
// 物化（内容源 → 磁盘）归应用——基座只持机制，内容归业务。

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
