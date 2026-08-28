// Package tstore 是基座测试用会话存储（tmpdir 目录实现 session.Store 与
// checkpoint.UserStore——布局约定同产品 FileStore：users/<op>/<rel> 子树 +
// .tmp 临时域 + 数据根）。仅测试消费（einox/internal 不可被模块外引用）。
package tstore

import (
	"os"
	"path/filepath"
	"sort"
)

// Store 目录式测试存储。
type Store struct {
	root string
}

// New 构造（root = t.TempDir()）。
func New(root string) *Store { return &Store{root: root} }

func (s *Store) userPath(operator, rel string) string {
	return filepath.Join(s.root, "users", operator, filepath.FromSlash(rel))
}

// ReadUserTreeFile 读用户域子树文件。
func (s *Store) ReadUserTreeFile(operator, rel string) ([]byte, bool) {
	b, err := os.ReadFile(s.userPath(operator, rel))
	return b, err == nil
}

// WriteUserTreeFile 写用户域子树文件（建目录 + 原子性从简——测试语义）。
func (s *Store) WriteUserTreeFile(operator, rel string, data []byte) error {
	p := s.userPath(operator, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// RemoveUserTree 删用户域子树。
func (s *Store) RemoveUserTree(operator, rel string) error {
	return os.RemoveAll(s.userPath(operator, rel))
}

// UserTreeDir 用户域根绝对路径（users/<op>；与 userPath 同根）。
func (s *Store) UserTreeDir(operator string) string {
	return filepath.Join(s.root, "users", operator)
}

// ListUserTreeSessions 列用户会话子目录。
func (s *Store) ListUserTreeSessions(operator string) []string {
	entries, err := os.ReadDir(filepath.Join(s.root, "users", operator, "sessions"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ListUsers 列用户（users/ 直下目录）。
func (s *Store) ListUsers() []string {
	entries, err := os.ReadDir(filepath.Join(s.root, "users"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TmpDir 临时域根。
func (s *Store) TmpDir() string { return filepath.Join(s.root, ".tmp") }

// Dir 数据根。
func (s *Store) Dir() string { return s.root }
