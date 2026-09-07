# 00 · 骨架样板(最小内核)

> `recipe: minimal` 的直出物——四必填装配 + 会话创建 + 一轮运行。本篇代码在 /tmp 实测工程 `go build ./...` 通过后落档;AI 生成内核时以此为底,按清单勾选项叠加其余 patterns 段。

**前置依赖**:无(最小形态)。

## 工程布局

```
my-agent/
  go.mod
  main.go        # 装配 + 入口(命令行参数 = 用户消息,跑一轮)
  filestore.go   # 演示级 session.Store(生产可替换业务实现)
```

## go.mod

```go
module example.com/my-agent

go 1.26.1

require github.com/jumeng/einox v0.0.0

replace github.com/jumeng/einox => /Users/ameng/Workspace/agent/einox
```

`replace` 是本地开发形态;发布后改为 `require github.com/jumeng/einox <版本>`(go.mod go 版本行与 einox 本仓一致)。首次构建 `go mod tidy` 补全间接依赖。

## filestore.go(演示级 session.Store)

布局同产品 FileStore:`users/<op>/<rel>` 子树 + `.tmp` 临时域。operator 围栏照 internal/tstore 同源规则——无效 operator 落隔离名,读写皆空转不出树。生产装配可整体替换为业务自己的 Store 实现(接口:`session.Store`,见 session/session.go)。

```go
package main

// fileStore 演示级会话存储(布局同产品 FileStore:users/<op>/<rel> 子树 +
// .tmp 临时域)。生产装配可替换为业务自己的 Store 实现(接口见
// session/session.go 的 Store)。operator 围栏照 internal/tstore 同源规则:
// 无效 operator 落隔离名,读写皆空转不出树。
import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fileStore struct{ root string }

func newFileStore(root string) *fileStore { return &fileStore{root: root} }

func fencedOperator(op string) string {
	if op == "" || op == "." || op == ".." ||
		strings.ContainsAny(op, `/\`) || strings.ContainsRune(op, 0) {
		return "_invalid-operator_"
	}
	return op
}

func (s *fileStore) userPath(operator, rel string) string {
	return filepath.Join(s.root, fencedOperator(operator), filepath.FromSlash(rel))
}

func (s *fileStore) ReadUserTreeFile(operator, rel string) ([]byte, bool) {
	b, err := os.ReadFile(s.userPath(operator, rel))
	return b, err == nil
}

func (s *fileStore) WriteUserTreeFile(operator, rel string, data []byte) error {
	p := s.userPath(operator, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (s *fileStore) RemoveUserTree(operator, rel string) error {
	return os.RemoveAll(s.userPath(operator, rel))
}

func (s *fileStore) UserTreeDir(operator string) string {
	return filepath.Join(s.root, fencedOperator(operator))
}

func (s *fileStore) ListUserTreeSessions(operator string) []string {
	entries, err := os.ReadDir(filepath.Join(s.root, fencedOperator(operator), "sessions"))
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

func (s *fileStore) ListUsers() []string {
	entries, err := os.ReadDir(s.root)
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

func (s *fileStore) TmpDir() string { return filepath.Join(s.root, ".tmp") }

func (s *fileStore) Dir() string { return s.root }
```

## main.go(最小装配)

四必填逐项标注;模型供应商内联单条(多供应商形态见 [model.md](model.md))。

```go
// my-agent —— einox 最小装配(四必填,recipe: minimal 的直出物)。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jumeng/einox/checkpoint"
	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/engine"
	"github.com/jumeng/einox/llm"
	"github.com/jumeng/einox/session"
)

func main() {
	dataDir := envOr("AGENT_DATA_DIR", "./data")
	store := newFileStore(filepath.Join(dataDir, "users"))
	reg := session.NewRegistry(store)

	m, err := engine.NewManager(reg, engine.Options{
		// ① Providers(必填):此处内联单供应商;多供应商/目录装配见 patterns/model.md
		Providers: func() []llm.ProviderSpec {
			return []llm.ProviderSpec{{
				ID:      "deepseek",
				Name:    "DeepSeek",
				Kind:    "openai",
				BaseURL: "https://api.deepseek.com",
				APIKey:  os.Getenv("DEEPSEEK_API_KEY"),
				Dialect: "deepseek",
				Enabled: true,
				Models:  []llm.ModelSpec{{ID: "deepseek-chat", Name: "DeepSeek Chat"}},
			}}
		},
		// ② Instruction(必填):业务职责段归应用
		Instruction: func(sess engine.SessionBrief) string {
			return "你是……(业务职责段)"
		},
		// ③ CheckPoints(必填)
		CheckPoints: func(operator, sid string) engine.CheckPointStore {
			return checkpoint.NewCheckPointStore(store, operator, sid)
		},
		// ④ WorkspaceRoot(必填)
		WorkspaceRoot: func(owner, sid string) string {
			return filepath.Join(dataDir, "workspaces", sid)
		},
	})
	if err != nil { // 装配错误启动期即拒
		fmt.Fprintln(os.Stderr, "装配失败:", err)
		os.Exit(1)
	}

	msg := strings.Join(os.Args[1:], " ")
	if msg == "" {
		fmt.Fprintln(os.Stderr, "用法: my-agent <用户消息>")
		os.Exit(2)
	}
	s := reg.Create("local", msg, "auto", contract.UserPrefs{Model: "deepseek/deepseek-chat"})
	m.Run(context.Background(), s, msg, nil, func(ev session.Event) {
		switch ev.Event {
		case contract.EvTextDelta:
			if d, ok := session.EventAs[contract.Delta](ev); ok {
				fmt.Print(d.Delta)
			}
		case contract.EvSessionEnd:
			fmt.Println()
		case contract.EvError:
			fmt.Fprintln(os.Stderr, "\n[error]", ev.Data)
		}
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

## 验证方法

```bash
go mod tidy && go build ./...   # 编译门(本篇样板的实测形态)
./my-agent 你好                  # 冒烟:stdout 出模型回复即回路通(需 DEEPSEEK_API_KEY)
```

零端点冒烟:`NewModel` 替换为 `llmtest.New(逐轮剧本…).Factory()`(测试假模型——`*llmtest.Model` 经 `Factory()` 包成 `llm.ModelFactory` 注入),剧本回一轮文本即可验证事件回路,不碰真实端点。

## 常见装配错误(启动期即拒,不拖到首会话)

| 报错 | 原因 |
|---|---|
| `engine: Options 缺必填项 Providers/…(不可为 nil)` | 四必填缺一 |
| `engine: 未知的会话域工具族 "xxx"` | `SessionToolsOff` 含未知名(合法族:todo/ask/plan/fs/cmd/patch) |
