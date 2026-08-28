// Package sandbox 提供 run_command 执行面的 OS 级沙箱机制（Phase S）：
// 策略模型（三档/网络开关/可写根/限额，平台无关）+ re-exec 哨兵协议
// （Landlock/seccomp/rlimit 必须 fork 后 exec 前在子进程内施加，Go self
// re-exec 惯例，codex helper 同构）+ 后端探测三态如实上报（dsh 式）。
// 机制归基座、选择归业务：应用装配层决定用不用/用哪档/参数（PM 默认关
// opt-in），基座承载实现与安全不变量。设计真源 = 仓内
// findings/2026-08-26-einox-sandbox-design.md（含 2026-08-26 独立审查修订）。
package sandbox

import (
	"fmt"
	"strings"
)

// Mode 策略三档。
type Mode string

const (
	ModeReadOnly         Mode = "read-only"          // 全盘只读
	ModeWorkspaceWrite   Mode = "workspace-write"    // 全盘读 + 工作区/临时目录/WritableRoots 写
	ModeDangerFullAccess Mode = "danger-full-access" // 全盘读写（仅资源限额与可选断网生效）
)

// Backend 后端姿态（探测不可用时的处置，真源 §1.3）。
type Backend string

const (
	BackendOff     Backend = "off"     // 不沙箱（Policy=nil 即此态，PM 默认）
	BackendAuto    Backend = "auto"    // 探测落位；不可用裸跑+告警（不隐式 fail-closed）
	BackendRequire Backend = "require" // 探测不可用即拒跑（fail-closed）
)

// Enforcement 执行力三态（真源 §1.2，dsh 式如实上报）。
type Enforcement string

const (
	EnforcementFull     Enforcement = "full"     // 目标策略全量生效
	EnforcementPartial  Enforcement = "partial"  // 生效但有未覆盖项（旧 ABI 少位等）
	EnforcementUnusable Enforcement = "unusable" // 后端不可用（未挂钩/内核不支持/平台未实现）
)

// Limit 资源限额（0 = 默认；helper 内 exec 前 setrlimit，真源 §2.4）。
type Limit struct {
	NProc      int // RLIMIT_NPROC；0 = 默认 512（内核按有效 uid 计数含线程，256 会被服务高线程+go build -p 打穿）
	FileSizeMB int // RLIMIT_FSIZE；0 = 默认 1024；单文件写上限，大产物场景调大
}

// 默认限额。
const (
	DefaultNProc      = 512
	DefaultFileSizeMB = 1024
)

// Policy 沙箱策略（对齐 codex legacy SandboxPolicy 形状 + 缓存重定向载体，
// 审查 A2 修订）。静态一次装配（真源 §7.5——PM 会话工作区固定，per-call 留演进）。
type Policy struct {
	Mode              Mode     // 三档
	Network           bool     // 默认 false 断网；LLM 调用在服务进程内不受影响
	WritableRoots     []string // workspace-write 档追加可写根（HOME 缓存类目录逃生门）
	Env               []string // 附加环境变量 "K=V"（缓存重定向载体，注入点 = cmd.Env）
	ExcludeTmpdir     bool     // true = $TMPDIR 不计入可写根（默认 false = 计入，审查 A1）
	ExcludeSlashTmp   bool     // true = /tmp 不计入可写根（默认 false = 计入）
	ProtectedReadOnly []string // 可写区内希望只读的子路径（Landlock 做不到——探测报 partial）
	Limit             Limit
}

// Validate 构造期 fail-fast（模式名唯一硬校验——其余字段语义自洽）。
func (p *Policy) Validate() error {
	switch p.Mode {
	case ModeReadOnly, ModeWorkspaceWrite, ModeDangerFullAccess:
		return nil
	default:
		return fmt.Errorf("sandbox: 未知模式 %q（可用 read-only/workspace-write/danger-full-access）", p.Mode)
	}
}

// Status 后端探测结果：三态 + partial 时的未覆盖项清单 + 诊断说明。
type Status struct {
	Enforcement Enforcement
	Uncovered   []string // 如 protected-readonly/refer/truncate/ioctl-dev
	Detail      string   // 人类可读诊断（含不可用原因）
}

// 拒绝方言注册表（真源 §1.4：每后端声明自己的拒绝签名，命中即错误信封附
// 提示行）。大小写不敏感匹配——Go errno 文案与 shell 文案大小写不一。
var denySignatures = map[string][]string{
	"landlock":  {"permission denied"},       // EACCES：文件系统围栏拒绝（Linux）
	"seccomp":   {"operation not permitted"}, // EPERM：断网/无条件 deny（Linux）
	"seatbelt":  {"operation not permitted"}, // Seatbelt 围栏拒绝（darwin）
	"win-token": {"access is denied"},        // ERROR_ACCESS_DENIED：restricted token 写拒绝（windows）
}

// denialHint 模型友好硬边界文案（nanobot WORKSPACE_BOUNDARY_NOTE 式措辞，
// 真源 §9.3）。escalation 本期不存在——文案指真实出口，禁「走审批」（C5）。
const denialHint = "沙箱拒绝：路径不可写或网络已断——这是硬边界，重试或换工具无效；如确需该操作请用 ask_user 向用户说明，由用户调整沙箱配置或人工执行"

// DenialHint 命中沙箱拒绝签名时返回提示行（空 = 未命中）。仅沙箱生效的
// 执行路径调用（裸跑降级路径命令输出与沙箱无关，不标注）。
func DenialHint(output string) string {
	lower := strings.ToLower(output)
	for _, pats := range denySignatures {
		for _, pat := range pats {
			if strings.Contains(lower, pat) {
				return denialHint
			}
		}
	}
	return ""
}
