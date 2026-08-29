//go:build windows

// Windows 后端（restricted token，真源 §4；codex windows-sandbox-rs 骨架
// 参照取最小集 = dsh sandbox-windows-acl 档）：CreateRestrictedToken
// （DISABLE_MAX_PRIVILEGE|LUA_TOKEN|WRITE_RESTRICTED 三 flags）+
// restricting SIDs = [logon, Everyone, 各可写根 cap SID（capsid.go 确定性
// 派生）] + 可写根 ACL allow-write ACE（standing——确定性 SID 使跨重启幂等
// 可复用）+ permissive default DACL（孙进程管道/IPC 不死，codex 同款）+
// CreateProcessAsUser 起子进程（os/exec SysProcAttr.Token 自动走）。
// 网络禁断不支持（WFP 需管理员装持久 provider，降档后置）——Enforcement
// 恒 partial + 未覆盖项 network，如实上报。全盘只读 = token 无 cap SID 即
// 全目录不可写（Everyone/硬链接边界为 dsh 文档化的本档已知残留）。
// 验证口径：交叉编译级 + SID 派生纯函数单测；本环境 Windows 宿主被
// Application Control 拦 go test，运行时未验（诚实声明，docs/09 同写）。
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreateRestrictedToken 为 x/sys 未封装件（v0.35.0），advapi32 raw 直调。
var procCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("CreateRestrictedToken")

// CreateRestrictedToken flags（winnt.h；codex token.rs 同组）。
const (
	disableMaxPrivilege = 0x01 // 剥全部特权
	luaToken            = 0x04 // 管理组 SID 转 deny-only
	writeRestricted     = 0x08 // restricting SIDs 只检查写访问
)

// 掩码常量（winnt.h；x/sys 未带文件语义位族——自携，landlock 属性结构同款
// 先例）。位级纪律（收官二轮审查 A-1，codex acl.rs WRITE_ALLOW_MASK 同款）：
// allow 掩码**不含 FILE_DELETE_CHILD**——父目录删子权授权会绕过子对象上的
// deny ACE（删除可经「对象自身 DELETE」或「父目录 FILE_DELETE_CHILD」二者
// 之一，后者不查询子对象 DACL），保护子路径会被整目录拔除后重建失守；
// rm/mv 不受影响（子对象经继承的 DELETE 自删）。deny 掩码取精确写位集并
// 含 DELETE|FILE_DELETE_CHILD（封两条删除路径）。
const (
	deleteRight        = 0x00010000 // DELETE
	fileDeleteChild    = 0x00000040 // FILE_DELETE_CHILD（目录删子权）
	fileGenericRead    = 0x00120089 // FILE_GENERIC_READ
	fileGenericWrite   = 0x00120116 // FILE_GENERIC_WRITE（含全部写数据位）
	fileGenericExecute = 0x001200A0 // FILE_GENERIC_EXECUTE

	writeAllowMask = fileGenericRead | fileGenericWrite | fileGenericExecute | deleteRight
	writeDenyMask  = fileGenericWrite | deleteRight | fileDeleteChild
)

// tokenDefaultDaclInfo TOKEN_DEFAULT_DACL（x/sys 未带该信息类结构体；
// 单指针字段布局自携）。
type tokenDefaultDaclInfo struct {
	DefaultDacl *windows.ACL
}

// tokenCache 进程级 token 缓存（静态一次装配假设——真源 §7.5；primary
// token 可复用于多次 CreateProcessAsUser）。键约束（收官审查 C-1）：只含
// mode|workspace|roots——同键不同 ProtectedReadOnly/Env/Network 的第二份
// 策略会复用旧 token 且跳过新 deny ACE；静态单策略假设下无此形态，per-call
// 策略演进时键须随之扩。
var tokenCache sync.Map // key: mode|workspace|roots → windows.Token

// AttachToken windows 侧挂：构造 restricted token 注入 cmd.SysProcAttr
// （Token 非零时 os/exec 自动走 CreateProcessAsUser）。danger 档不加 fs
// 围栏（语义=全盘读写；资源限额 Windows 无 setrlimit 等价面，诚实记档）。
func AttachToken(cmd *exec.Cmd, pol *Policy, workspace string) error {
	if pol == nil || pol.Mode == ModeDangerFullAccess {
		return nil
	}
	tok, err := buildRestrictedToken(pol, workspace)
	if err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(tok)}
	return nil
}

// wrapOSBackend windows 分支：命令直执行（不经哨兵——围栏全在父进程侧的
// token+ACL，子进程内无施加步骤），token 由 AttachToken 挂（runcommand
// buildCmd 在构造 cmd 后调用）。env 过 cleanseEnv 剥 LLM_* 凭证面（收官
// 审查 B-1：linux/darwin 经哨兵 helperExec 清洗，windows 直执行路径须
// 同款——C-3 加固跨平台一致）。
func wrapOSBackend(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	return []string{"sh", "-c", cmdLine}, cleanseEnv(mergeEnv(baseEnv(pol), pol.Env))
}

// applySandbox windows：哨兵路径不经（围栏在父进程侧），误入即报错拒绝。
func applySandbox(*policyPayload) error {
	return fmt.Errorf("windows 后端不经哨兵施加（token 在父进程侧装配）")
}

// probeEnforceChild 后端实测（真源 §1.3 ②）：临时工作区 + workspace-write
// token 自测——围栏外写须被拒（根外路径无 cap ACE，pass-2 失败）+ 根内写
// 须成功；网络禁断不支持 → partial + uncovered=network（协议第 4 段，
// probeOSBackend 解析）。ExcludeTmpdir 必置（2026-08-27 收官审查 A-1）：
// 工作区即 %TEMP% 下随机子目录，不排除 temp 根则「根外写」探针目标恰在
// 已授权的 %TEMP% 共享根内——写必成功、探针恒报围栏失效 → Probe unusable
// → windows 沙箱全灭。
func probeEnforceChild() int {
	dir, err := os.MkdirTemp("", "einox-sbx-probe-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: %v\n", err)
		return helperFail
	}
	defer os.RemoveAll(dir)
	tok, err := buildRestrictedToken(&Policy{Mode: ModeWorkspaceWrite, ExcludeTmpdir: true}, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "einox-sandbox: token 自测构造失败: %v\n", err)
		return helperFail
	}
	out := filepath.Join(os.TempDir(), fmt.Sprintf("einox-sbx-probe-out-%d", os.Getpid()))
	_ = os.Remove(out)
	defer os.Remove(out)
	if code := runProbeWrite(tok, out); code != 1 {
		fmt.Fprintf(os.Stderr, "einox-sandbox: 围栏未拒绝根外写（探针退出码 %d）——围栏失效\n", code)
		return helperFail
	}
	if code := runProbeWrite(tok, filepath.Join(dir, "probe.txt")); code != 0 {
		fmt.Fprintf(os.Stderr, "einox-sandbox: 根内写被拒（探针退出码 %d）——围栏过紧\n", code)
		return helperFail
	}
	fmt.Println("einox-sandbox-enforce partial abi=0 uncovered=network")
	return 0
}

// runProbeWrite 围栏内替身执行（restricted token 起探针孙进程）：返回探针
// 退出码（0=写成功/1=写被拒），spawn 失败返回 -1。
func runProbeWrite(tok windows.Token, path string) int {
	cmd := exec.Command(selfExe, sentinelCmd, flagProbeWrite, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(tok)}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return -1
	}
	return 0
}

// buildRestrictedToken 构造 restricted token + 可写根 ACE 授予（缓存复用）。
// 失败即整体失败（auto 档裸跑降级由调用方处置）。
func buildRestrictedToken(pol *Policy, workspace string) (windows.Token, error) {
	var roots []string
	if pol.Mode == ModeWorkspaceWrite {
		roots = append([]string{workspace}, pol.WritableRoots...)
		if !pol.ExcludeTmpdir {
			// 默认计入（审查 A1 同源语义）。有意取舍（真源 §11.10 记档）：授权
			// 整个共享 %TEMP% 根而非 dsh 的每会话私有 temp 目录+独立 SID——对齐
			// Linux /tmp 世界可写语义；代价 = 同机各会话/进程可互写 temp 树（dsh
			// 明确以此为忌），内网单用户形态下接受。
			roots = append(roots, os.TempDir())
		}
	}
	key := string(pol.Mode) + "|" + canonicalPathKey(workspace) + "|" +
		strings.Join(mapStr(roots, canonicalPathKey), ",")
	if v, ok := tokenCache.Load(key); ok {
		return v.(windows.Token), nil
	}
	base, err := openCurrentProcessToken()
	if err != nil {
		return 0, err
	}
	defer base.Close()
	logon, err := logonSID(base)
	if err != nil {
		return 0, err
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, err
	}
	var caps []*windows.SID
	seen := map[string]bool{}
	for _, root := range roots {
		root = filepath.Clean(root)
		ck := canonicalPathKey(root)
		if seen[ck] {
			continue
		}
		seen[ck] = true
		sid, err := windows.StringToSid(WorkspaceWriteSID(root))
		if err != nil {
			return 0, fmt.Errorf("cap SID 构造: %w", err)
		}
		if err := grantWriteACE(root, sid); err != nil {
			return 0, fmt.Errorf("可写根 %s 授权失败: %w", root, err)
		}
		caps = append(caps, sid)
	}
	// ProtectedReadOnly：可写区内子路径 deny-write ACE（codex
	// protect_workspace_subdir 同款——deny ACE 在 WRITE_RESTRICTED 语义下
	// 对全部 granting SID 生效，即真回盖）。
	for _, prot := range pol.ProtectedReadOnly {
		if _, err := os.Lstat(prot); err != nil {
			continue // 不存在 = 无可保护对象（授权侧 RequireNot 双形语义此处无对应需求）
		}
		if err := denyWriteACE(prot, caps...); err != nil {
			return 0, fmt.Errorf("保护子路径 %s: %w", prot, err)
		}
	}
	restrict := append([]*windows.SID{logon, world}, caps...)
	tok, err := createRestrictedToken(base, restrict)
	if err != nil {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", err)
	}
	if err := setDefaultDacl(tok, restrict); err != nil {
		tok.Close()
		return 0, fmt.Errorf("default DACL: %w", err)
	}
	tokenCache.Store(key, tok)
	return tok, nil
}

func mapStr(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

// openCurrentProcessToken 当前进程 token（CreateRestrictedToken 所需权限面）。
func openCurrentProcessToken() (windows.Token, error) {
	var tok windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT,
		&tok)
	return tok, err
}

// logonSID token 组内的 logon 会话 SID（S-1-5-5-x-y，SE_GROUP_LOGON_ID 位）：
// WinSta0/桌面等 per-logon 对象访问需要它在 restricting 列表内。
func logonSID(tok windows.Token) (*windows.SID, error) {
	groups, err := tok.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			return g.Sid, nil
		}
	}
	return nil, fmt.Errorf("token 无 logon SID")
}

// createRestrictedToken raw 直调（x/sys v0.35.0 未封装）。
func createRestrictedToken(base windows.Token, restrictSIDs []*windows.SID) (windows.Token, error) {
	entries := make([]windows.SIDAndAttributes, len(restrictSIDs))
	for i, sid := range restrictSIDs {
		entries[i].Sid = sid
	}
	var newToken windows.Token
	r1, _, callErr := procCreateRestrictedToken.Call(
		uintptr(base),
		disableMaxPrivilege|luaToken|writeRestricted,
		0, 0, // 无 delete SIDs
		0, 0, // 无 disable privileges（DISABLE_MAX_PRIVILEGE 已全剥）
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r1 == 0 {
		return 0, callErr
	}
	return newToken, nil
}

// sidTrustee SID 形 trustee（EXPLICIT_ACCESS 用）。
func sidTrustee(sid *windows.SID) windows.TRUSTEE {
	return windows.TRUSTEE{
		TrusteeForm:  windows.TRUSTEE_IS_SID,
		TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
		TrusteeValue: windows.TrusteeValue(unsafe.Pointer(sid)),
	}
}

// setDefaultDacl 合并 permissive default DACL（restricting SID 全量 GENERIC_ALL
// ACE）：restricted token 继承的用户 default DACL 不含 restricting SID——
// 孙进程建匿名管道（stdio）会在对象创建的 pass-2 检查上 ACCESS_DENIED
// （codex/dsh 同款坑，管道 IPC 全灭）。
func setDefaultDacl(tok windows.Token, sids []*windows.SID) error {
	var size uint32
	windows.GetTokenInformation(tok, windows.TokenDefaultDacl, nil, 0, &size) // 预期 insufficient buffer
	if size == 0 {
		return fmt.Errorf("TokenDefaultDacl 尺寸查询返回 0")
	}
	buf := make([]byte, size)
	if err := windows.GetTokenInformation(tok, windows.TokenDefaultDacl, &buf[0], size, &size); err != nil {
		return err
	}
	old := (*tokenDefaultDaclInfo)(unsafe.Pointer(&buf[0])).DefaultDacl
	entries := make([]windows.EXPLICIT_ACCESS, len(sids))
	for i, sid := range sids {
		entries[i] = windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee:           sidTrustee(sid),
		}
	}
	dacl, err := windows.ACLFromEntries(entries, old)
	if err != nil {
		return err
	}
	info := tokenDefaultDaclInfo{DefaultDacl: dacl}
	return windows.SetTokenInformation(tok, windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// grantWriteACE 目录 DACL 加 cap SID 的 allow-write ACE（子树继承；与既有
// DACL 合并非替换）。确定性 SID + SetEntriesInAcl 对同 trustee 的合并语义
// → 跨进程重启幂等（重复授予不堆积语义变化）。
func grantWriteACE(dir string, sid *windows.SID) error {
	return mutateDACL(dir, windows.EXPLICIT_ACCESS{
		AccessPermissions: writeAllowMask,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee:           sidTrustee(sid),
	})
}

// denyWriteACE 路径 DACL 加 deny-write ACE（对全部 caps 生效——任何授权
// SID 都不得经此路径写；子树继承）。
func denyWriteACE(path string, caps ...*windows.SID) error {
	for _, sid := range caps {
		if err := mutateDACL(path, windows.EXPLICIT_ACCESS{
			AccessPermissions: writeDenyMask,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee:           sidTrustee(sid),
		}); err != nil {
			return err
		}
	}
	return nil
}

// mutateDACL 取既有 DACL → SetEntriesInAcl 合并 → 写回（替换会抹掉既有
// ACE——合并是保命语义）。
func mutateDACL(path string, entry windows.EXPLICIT_ACCESS) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("GetNamedSecurityInfo: %w", err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("DACL 读取: %w", err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, old)
	if err != nil {
		return fmt.Errorf("ACLFromEntries: %w", err)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
