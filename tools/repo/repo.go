// Package repo 是代码仓挂载与 git 面工具族（会话域件）：open_repo 把采集缓存
// 仓以 git worktree 挂载进会话工作区 repos/<短名>/（任务分支 agent/<sid>-<n>，
// 会话级持久——工作区任务收尾清理排除 repos/）；status/diff 只读面、commit 写
// 面（应用层 ArgsForce 级人工确认）与补丁导出。git 经二进制 exec、参数走
// argv 不经 shell；仓定位由应用注入 Resolver（清单来自业务配置，不开放任意
// 路径 clone）。
package repo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
)

// Resolver 仓定位（短名 → 缓存目录 + 默认基线 ref；ok=false = 未登记）。
type Resolver interface {
	Resolve(name string) (dir, ref string, ok bool)
}

// Config 构造配置。
type Config struct {
	Resolver    Resolver
	Root        string                                                   // 会话工作区根（挂载点 <Root>/repos/<短名>）
	Owner, SID  string                                                   // 会话标识（分支命名 agent/<sid>-<n>）
	PatchWriter func(name string, content []byte, operator string) error // 补丁导出落盘（docs/exports/）；nil = 导出面报未配置
}

type openIn struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

type nameIn struct {
	Name string `json:"name"`
}

type diffIn struct {
	Name string `json:"name"`
	Path string `json:"path"` // 可选过滤（限定单文件/子目录）
}

type commitIn struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// NewTools 构造五件（open_repo/repo_status/repo_diff/repo_commit/export_patch；
// repo_commit 包 DiffToolOf 暴露审批卡 diff 载荷）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	if cfg.Resolver == nil || cfg.Root == "" || cfg.SID == "" {
		return nil, fmt.Errorf("repo 工具族未装配（Resolver/Root/SID 缺失）")
	}
	open, err := tools.InferTool("open_repo",
		"挂载代码仓到会话工作区（git worktree，任务分支 agent/<会话>-N；ref 基线缺省 = 仓配置分支/默认分支）。name = 仓短名（见系统提示词仓清单）。挂载后 repos/<短名>/ 即工作区内普通路径：read_file / search_files / apply_patch / run_command 直接用相对路径访问。重复挂载幂等返回当前分支状态（基线以首次挂载为准，换基线需新开会话）。",
		func(ctx context.Context, in openIn) (map[string]any, error) { return openRepo(cfg, in) })
	if err != nil {
		return nil, err
	}
	status, err := tools.InferTool("repo_status",
		"查看挂载仓状态：当前分支、相对基线 ahead/behind、工作区改动清单（porcelain 解析，最多 500 条）。name = 仓短名；仓未挂载先 open_repo。",
		func(ctx context.Context, in nameIn) (map[string]any, error) { return repoStatus(cfg, in) })
	if err != nil {
		return nil, err
	}
	diff, err := tools.InferTool("repo_diff",
		"查看挂载仓改动 diff（merge-base 基线 → 工作区，含未提交改动）。name = 仓短名；path 可选，限定单文件/子目录（输出超长截断时按 path 分文件看）。",
		func(ctx context.Context, in diffIn) (map[string]any, error) { return repoDiff(cfg, in) })
	if err != nil {
		return nil, err
	}
	commit, err := tools.InferTool("repo_commit",
		"提交挂载仓全部改动（git add -A + commit）：message = 提交说明。无改动拒绝提交（先 repo_status 查看）；已提交历史供 export_patch 导出补丁。",
		func(ctx context.Context, in commitIn) (map[string]any, error) { return repoCommit(cfg, in) })
	if err != nil {
		return nil, err
	}
	commit = tools.DiffToolOf(commit, commitCardDiff(cfg))
	export, err := tools.InferTool("export_patch",
		"把挂载仓已提交改动导出为补丁文件（git format-patch，落文档区 docs/exports/）。name = 仓短名；无已提交改动拒绝导出（先 repo_commit）。",
		func(ctx context.Context, in nameIn) (map[string]any, error) { return exportPatch(ctx, cfg, in) })
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(open, contract.BehaviorExec), tools.WithBehavior(status, contract.BehaviorRead), tools.WithBehavior(diff, contract.BehaviorRead), tools.WithBehavior(commit, contract.BehaviorWrite), tools.WithBehavior(export, contract.BehaviorRead)}, nil
}

// validName 仓短名围栏：仅作 repos/<短名> 目录段，防路径穿越（含 / \ .. 一律拒绝）。
func validName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// mounted 取挂载仓执行面：短名围栏 + 挂载存在 + 基线 ref（Resolver 回查）。
func mounted(cfg Config, name string) (mount, ref string, err error) {
	if !validName(name) {
		return "", "", fmt.Errorf("非法仓短名：%s", name)
	}
	if !cfg.mountExists(name) {
		return "", "", fmt.Errorf("仓未挂载，先 open_repo")
	}
	_, ref, ok := cfg.Resolver.Resolve(name)
	if !ok {
		return "", "", fmt.Errorf("未登记的仓：%s（可用仓见系统提示词仓清单）", name)
	}
	return cfg.mountDir(name), ref, nil
}

// openRepo 挂载（幂等）：Resolve → 基线定档（in.Ref 覆盖）→ 找空闲任务分支号
// → worktree add → worktree 级禁 push。
func openRepo(cfg Config, in openIn) (map[string]any, error) {
	if !validName(in.Name) {
		return fail("非法仓短名：" + in.Name + "（仅作 repos/<短名> 目录段，不得含路径分隔符或 ..）")
	}
	dir, ref, ok := cfg.Resolver.Resolve(in.Name)
	if !ok {
		return fail("未登记的仓：" + in.Name + "（可用仓见系统提示词仓清单）")
	}
	if in.Ref != "" { // 覆盖基线（防御性拒绝空白/以 - 开头，防 argv 误解析）
		ref = strings.TrimSpace(in.Ref)
		if ref == "" || strings.HasPrefix(ref, "-") {
			return fail("非法 ref：" + in.Ref + "（空白或以 - 开头）")
		}
	}
	mount := cfg.mountDir(in.Name)
	if cfg.mountExists(in.Name) { // 幂等分支：返回当前分支状态（基线以首次挂载为准）
		out, err := gitOut(mount, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fail(err.Error())
		}
		res := map[string]any{
			"ok": true, "name": in.Name, "path": "repos/" + in.Name,
			"branch": strings.TrimSpace(out), "base": ref, "note": "已挂载（幂等返回）",
		}
		if a, b, err := aheadBehind(mount, ref); err == nil {
			res["ahead"], res["behind"] = a, b
		}
		return res, nil
	}
	var branch string
	for n := 1; ; n++ { // 找空闲任务分支号：rev-parse 报错即不存在 → 空闲
		branch = fmt.Sprintf("agent/%s-%d", cfg.SID, n)
		if _, err := gitOut(dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err != nil {
			break
		}
	}
	if _, err := gitOut(dir, "worktree", "add", "-b", branch, mount, ref); err != nil {
		return fail("挂载失败：" + err.Error() + "（基线 " + ref + " 不存在则仓未采集，先用 fetch_and_collect）")
	}
	// worktree 级禁 push：remote.origin.pushurl 指向不可用协议——「不 push」
	// 从提示词约束落回硬约束（fetch 走 origin.url 不受影响）。配置失败不阻断
	// 挂载（内网无认证 push 低概率面，提示词红线仍在）。
	_, _ = gitOut(mount, "config", "extensions.worktreeConfig", "true")
	_, _ = gitOut(mount, "config", "--worktree", "remote.origin.pushurl", "no-push://disabled")
	return map[string]any{
		"ok": true, "name": in.Name, "path": "repos/" + in.Name,
		"branch": branch, "base": ref,
		"note": "已挂载，repos/" + in.Name + "/ 下即仓内容",
	}, nil
}

// repoStatus 只读状态面：分支 / ahead-behind / 改动清单。
func repoStatus(cfg Config, in nameIn) (map[string]any, error) {
	mount, ref, err := mounted(cfg, in.Name)
	if err != nil {
		return fail(err.Error())
	}
	out, err := gitOut(mount, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fail(err.Error())
	}
	if _, _, err := mergeBase(mount, ref); err != nil { // 基线存在性前置（未采集即时报错）
		return fail(err.Error())
	}
	ahead, behind, err := aheadBehind(mount, ref)
	if err != nil {
		return fail(err.Error())
	}
	changes, truncated, err := statusChanges(mount)
	if err != nil {
		return fail(err.Error())
	}
	return map[string]any{
		"ok": true, "branch": strings.TrimSpace(out), "base": ref,
		"ahead": ahead, "behind": behind, "changes": changes, "truncated": truncated,
	}, nil
}

// statusChanges porcelain 改动清单（状态 = 前 2 字符 trim，路径 = 第 4 字符起；
// 最多 500 条 + truncated）。
func statusChanges(mount string) ([]map[string]string, bool, error) {
	out, err := gitOut(mount, "status", "--porcelain")
	if err != nil {
		return nil, false, err
	}
	lines := strings.Split(out, "\n") // 不整体 TrimSpace——porcelain 行首可为状态空格（如 " M"）
	changes := make([]map[string]string, 0, len(lines))
	truncated := false
	for _, ln := range lines {
		if len(ln) < 4 { // 空行/尾行跳过
			continue
		}
		if len(changes) >= 500 {
			truncated = true
			break
		}
		changes = append(changes, map[string]string{"status": strings.TrimSpace(ln[:2]), "path": ln[3:]})
	}
	return changes, truncated, nil
}

// repoDiff 基线 → 工作区 diff（超 8000 rune 截断附提示）。
func repoDiff(cfg Config, in diffIn) (map[string]any, error) {
	mount, ref, err := mounted(cfg, in.Name)
	if err != nil {
		return fail(err.Error())
	}
	out, err := diffOf(mount, ref, strings.TrimSpace(in.Path))
	if err != nil {
		return fail(err.Error())
	}
	if r := []rune(out); len(r) > 8000 {
		out = string(r[:8000]) + "…（超长截断——可按 path 参数分文件看）"
	}
	return map[string]any{"ok": true, "diff": out}, nil
}

// repoCommit 写面：全量 add + commit（无改动拒绝）。
func repoCommit(cfg Config, in commitIn) (map[string]any, error) {
	mount, _, err := mounted(cfg, in.Name)
	if err != nil {
		return fail(err.Error())
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		return fail("message 不能为空")
	}
	changes, _, err := statusChanges(mount)
	if err != nil {
		return fail(err.Error())
	}
	if len(changes) == 0 {
		return fail("无改动可提交（repo_status 查看）")
	}
	if _, err := gitOut(mount, "add", "-A"); err != nil {
		return fail(err.Error())
	}
	if _, err := gitOut(mount, "commit", "-m", msg); err != nil {
		return fail(err.Error())
	}
	hash, err := gitOut(mount, "rev-parse", "--short", "HEAD")
	if err != nil {
		return fail(err.Error())
	}
	branch, err := gitOut(mount, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fail(err.Error())
	}
	// B4 文件变更信封：本次提交 ±行数（numstat 汇总；binary 行 "-" 跳过）
	add, del := 0, 0
	if ns, err := gitOut(mount, "show", "--numstat", "--format=", "HEAD"); err == nil {
		for _, l := range strings.Split(ns, "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			if n, e := strconv.Atoi(f[0]); e == nil {
				add += n
			}
			if n, e := strconv.Atoi(f[1]); e == nil {
				del += n
			}
		}
	}
	return map[string]any{
		"ok": true, "commit": strings.TrimSpace(hash),
		"branch": strings.TrimSpace(branch), "changed": len(changes),
		"counts": fmt.Sprintf("+%d -%d", add, del),
	}, nil
}

// exportPatch 已提交改动 → format-patch → PatchWriter（文档区 exports/）。
func exportPatch(ctx context.Context, cfg Config, in nameIn) (map[string]any, error) {
	mount, ref, err := mounted(cfg, in.Name)
	if err != nil {
		return fail(err.Error())
	}
	if cfg.PatchWriter == nil {
		return fail("导出面未配置")
	}
	mb, _, err := mergeBase(mount, ref)
	if err != nil {
		return fail(err.Error())
	}
	out, err := gitOut(mount, "format-patch", "--stdout", mb+"..HEAD")
	if err != nil {
		return fail(err.Error())
	}
	if strings.TrimSpace(out) == "" {
		return fail("无已提交改动（先 repo_commit）")
	}
	branchOut, err := gitOut(mount, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fail(err.Error())
	}
	branch := strings.TrimSpace(branchOut)
	fname := fmt.Sprintf("%s-%s-%s.patch", in.Name, strings.ReplaceAll(branch, "/", "-"), time.Now().Format("20060102"))
	if err := cfg.PatchWriter(fname, []byte(out), contract.OperatorOf(ctx)); err != nil {
		return fail("导出失败：" + err.Error())
	}
	return map[string]any{
		"ok": true, "file": "docs/exports/" + fname, "note": "补丁已落文档区 exports/",
	}, nil
}

// commitCardDiff repo_commit 审批卡 diff 载荷：解析 {"name":…} → 挂载存在则
// 复用 repo_diff 核心 + 追加未跟踪新文件清单（add -A 会纳入而 git diff 不含，
// 卡面完整性）；diff 本体按 12000 减清单余量截断，清单段恒存活（总量 12000
// 兜底）；失败/未挂载返回 ""（卡面降级无 diff）。
func commitCardDiff(cfg Config) func(args string) string {
	return func(args string) string {
		var in struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || !validName(in.Name) || !cfg.mountExists(in.Name) {
			return ""
		}
		_, ref, ok := cfg.Resolver.Resolve(in.Name)
		if !ok {
			return ""
		}
		mount := cfg.mountDir(in.Name)
		out, err := diffOf(mount, ref, "")
		if err != nil {
			return ""
		}
		// untracked 清单先取并计预算：diff 本体按余量截断——总量统一截断会
		// 在 diff 超长时整段吃掉尾部清单（add -A 会提交这些文件，人审盲区）
		untracked := untrackedList(mount)
		if budget := 12000 - len([]rune(untracked)) - 100; budget > 0 {
			out = truncateRunes(out, budget)
		}
		return truncateRunes(out+untracked, 12000)
	}
}

// fail 统一失败信封（回喂模型自纠，errFeed 语义）。
func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil
}
