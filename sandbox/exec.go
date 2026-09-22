// 单命令沙箱化构造单点（D1 require 姿态接线，设计参照 2026-09-22 审查
// findings/2026-09-22-base-audit.md §6 D1/D2）：此前散在 runcommand.buildCmd
// （Policy 无 Backend 字段，后端不可用一律 auto 语义裸跑——三处「接线留待
// 首个 fail-closed 消费者」注释的收口）。require 姿态两处拒跑点：Wrap 返回
// nil argv（后端不可用/探测 unusable）与 OS 后端 token 构造失败；auto 姿态
// 维持现状（告警一次 + 裸跑，env 治理不回退）。extwire localOperator
// （python_execute 内层执行面，缝隙①）同经此构造收口。
package sandbox

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// tokenBuildFailWarn token 构造失败告警（进程一次——自 runcommand 下沉，
// 单点节流；require 姿态不告警直接拒）。
var tokenBuildFailWarn sync.Once

// BuildCommand 单命令沙箱化构造。workspace = 沙箱写域根（Wrap/AttachToken
// 的策略锚——run_command 形态即工作区根）；dir = 进程工作目录（空 = 继承
// 调用方 cwd）；pol 非 nil 即尝试沙箱（nil = 直执行零变化）；argv = 直执行
// 参数（无 shell 语义——经 Provider 的 cmdLine 通道时逐元素 POSIX 单引号
// 包裹，空格/元字符安全，单引号内禁一切展开；哨兵协议 sh -c 解析还原等价
// argv）。extraEnv = 已校验的 K=V 注入（沙箱形态 append 到 provider 治理
// env 尾部，裸跑形态 append 到继承环境）。第二返回值 = 是否沙箱生效（拒绝
// 提示标注仅该路径）。err 非 nil = require 姿态 fail-closed 拒跑（调用方
// 转错误信封，不降级）。
func BuildCommand(ctx context.Context, workspace, dir string, pol *Policy, p Provider, argv, extraEnv []string) (*exec.Cmd, bool, error) {
	if len(argv) == 0 {
		return nil, false, fmt.Errorf("sandbox: 空 argv")
	}
	if pol == nil {
		return bareCmd(ctx, dir, argv, extraEnv), false, nil
	}
	if p == nil {
		p = OSProvider
	}
	if wrapped, env := p.Wrap(pol, workspace, shellQuoteJoin(argv)); wrapped != nil {
		cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
		cmd.Dir = dir
		cmd.Env = append(env, extraEnv...)
		SetGroupLeader(cmd)
		if p == OSProvider {
			if err := AttachToken(cmd, pol, workspace); err != nil {
				if pol.Backend == BackendRequire {
					return nil, false, fmt.Errorf("沙箱 require 档：token 构造失败（%v）——拒绝执行（fail-closed）", err)
				}
				// windows restricted token 构造失败：裸跑降级会失去围栏——
				// 静默 fail-open 不可接受，告警一次（auto 语义）
				tokenBuildFailWarn.Do(func() {
					log.Printf("sandbox: token 构造失败（%v）——该命令裸跑（auto 档降级；require 档将拒跑）", err)
				})
				return wrappedBare(ctx, dir, argv, env, extraEnv), false, nil
			}
		}
		cmd.Cancel = func() error { // 超时/取消通道同款进程组杀
			KillGroup(cmd.Process)
			return nil
		}
		return cmd, true, nil
	}
	// Wrap 返回 nil argv = 本次不可沙箱（后端不可用/容器后端未就绪）
	if pol.Backend == BackendRequire {
		st := p.Probe()
		return nil, false, fmt.Errorf("沙箱 require 档：后端不可用（%s）——拒绝执行（fail-closed）", st.Detail)
	}
	// auto（缺省零值同义）：裸跑降级（探测已告警一次——osProvider.Wrap 的
	// unusable 路径；容器后端自担告警）
	return bareCmd(ctx, dir, argv, extraEnv), false, nil
}

// bareCmd 直执行（无 shell 语义；进程组纪律同沙箱路径——超时/停止整组终结）。
func bareCmd(ctx context.Context, dir string, argv, extraEnv []string) *exec.Cmd {
	return wrappedBare(ctx, dir, argv, nil, extraEnv)
}

// wrappedBare 裸跑构造（envBase 非 nil = 沙箱 env 治理保留形态——token
// 降级路径不回退到继承全量环境）。
func wrappedBare(ctx context.Context, dir string, argv, envBase, extraEnv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if envBase != nil {
		cmd.Env = append(envBase, extraEnv...)
	} else if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	SetGroupLeader(cmd)
	cmd.Cancel = func() error {
		KillGroup(cmd.Process)
		return nil
	}
	return cmd
}

// shellQuoteJoin argv → cmdLine（逐元素 POSIX 单引号包裹：内部 ' 转义 '\”；
// 单引号内禁参数展开与通配——还原等价 argv，空格/元字符路径安全）。
func shellQuoteJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}
