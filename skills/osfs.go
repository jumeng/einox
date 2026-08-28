package skills

// osBackend filesystem.Backend 的只读 OS 实现（skill middleware 的
// BackendFromFilesystem 需要一个 Backend——官方仅附 in-memory 实现）。
// 语义：LsInfo/Read/GlobInfo/GrepRaw 真实现；Write/Edit 拒绝（物化区只读，
// docs/04 定界——skill 是产品资产，修改路径是产品仓而非运行时）。

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudwego/eino/adk/filesystem"
)

type osBackend struct{}

// fileMode 格式化 mtime（ISO 8601 简式）。
func fileMode(fi os.FileInfo) string {
	return fi.ModTime().UTC().Format(time.RFC3339)
}

func (osBackend) LsInfo(_ context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	entries, err := os.ReadDir(req.Path)
	if err != nil {
		return nil, err
	}
	out := make([]filesystem.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, filesystem.FileInfo{
			Path: e.Name(), IsDir: e.IsDir(), Size: info.Size(), ModifiedAt: fileMode(info),
		})
	}
	return out, nil
}

func (osBackend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	data, err := os.ReadFile(req.FilePath)
	if err != nil {
		return nil, err
	}
	content := string(data)
	if req.Offset > 1 || req.Limit > 0 {
		lines := strings.Split(content, "\n")
		off := req.Offset
		if off < 1 {
			off = 1
		}
		if off > len(lines) {
			off = len(lines)
		}
		end := len(lines)
		if req.Limit > 0 && off-1+req.Limit < end {
			end = off - 1 + req.Limit
		}
		content = strings.Join(lines[off-1:end], "\n")
	}
	return &filesystem.FileContent{Content: content}, nil
}

func (osBackend) GlobInfo(_ context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	var out []filesystem.FileInfo
	err := filepath.WalkDir(req.Path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(req.Path, p)
		if rerr != nil {
			return nil
		}
		if ok, _ := doublestar.Match(req.Pattern, rel); ok {
			info, ierr := d.Info()
			if ierr == nil {
				out = append(out, filesystem.FileInfo{
					Path: rel, IsDir: d.IsDir(), Size: info.Size(), ModifiedAt: fileMode(info),
				})
			}
		}
		return nil
	})
	return out, err
}

func (osBackend) GrepRaw(_ context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		return nil, err
	}
	var out []filesystem.GrepMatch
	root := req.Path
	if root == "" {
		root = "."
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		if req.Glob != "" {
			if ok, _ := doublestar.Match(req.Glob, p); !ok {
				return nil
			}
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			hit := re.MatchString(text)
			if hit && req.CaseInsensitive {
				hit = true //regexp 已编译则忽略大小写由 (?i) 控制;简化处理
			}
			if hit {
				out = append(out, filesystem.GrepMatch{Content: text, Path: p, Line: line})
			}
		}
		return nil
	})
	return out, nil
}

func (osBackend) Write(_ context.Context, _ *filesystem.WriteRequest) error {
	return fmt.Errorf("只读物化区：skill 系统文件不可运行时写（修改走产品仓 skills/）")
}

func (osBackend) Edit(_ context.Context, _ *filesystem.EditRequest) error {
	return fmt.Errorf("只读物化区：skill 系统文件不可运行时编辑（修改走产品仓 skills/）")
}
