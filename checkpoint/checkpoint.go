// Package checkpoint 是会话检查点的文件存储（adk Get/Set 两方法；自产品
// internal/agent/checkpoint.go 迁入）。落位 .agent/users/<operator>/sessions/
// <sid>/checkpoints/<key>（用户隔离 + 会话删除整目录同删）；存储经 UserStore
// 接口注入——应用数据面（唯一文件写入器）在产品侧。
package checkpoint

// CheckPointStore 文件实现（adk Get/Set 两方法；M3-0 核实④）。
// 落位 .agent/users/<operator>/sessions/<sid>/checkpoints/<key>
// （用户隔离 + 会话删除整目录同删）。key 做文件名安全化（adk key 为 agent
// 标识/路径段，保守替换非法字符）。

import (
	"context"
	"path"
	"strings"
)

// UserStore 用户域子树读写面（应用注入——产品 FileStore 结构性满足）。
type UserStore interface {
	ReadUserTreeFile(operator, rel string) ([]byte, bool)
	WriteUserTreeFile(operator, rel string, data []byte) error
}

// FileCheckPointStore 单会话的 checkpoint 存储（operator+sid 定位用户域子树）。
type FileCheckPointStore struct {
	st       UserStore
	operator string
	sid      string
}

// NewCheckPointStore 构造（operator/sid 已由会话域校验过形态）。
func NewCheckPointStore(st UserStore, operator, sid string) *FileCheckPointStore {
	return &FileCheckPointStore{st: st, operator: operator, sid: sid}
}

// safeKey checkpoint key → 文件名（白名单字符外替换 '_'，防路径注入）。
func safeKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "default"
	}
	return out
}

// Get 读 checkpoint（不存在 = ok false）。
func (s *FileCheckPointStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	data, ok := s.st.ReadUserTreeFile(s.operator, path.Join("sessions", s.sid, "checkpoints", safeKey(key)+".bin"))
	if !ok {
		return nil, false, nil
	}
	return data, true, nil
}

// Set 写 checkpoint（走 store 唯一写入器串行队列 + 原子写）。
func (s *FileCheckPointStore) Set(_ context.Context, key string, checkpoint []byte) error {
	return s.st.WriteUserTreeFile(s.operator, path.Join("sessions", s.sid, "checkpoints", safeKey(key)+".bin"), checkpoint)
}
