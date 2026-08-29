// DockerProvider Docker 一次性容器后端（dockerWrap 正规化迁移——既有
// EINO_RUN_DOCKER env 魔法开关与「绕过 policy」优先级告警退役，策略翻译
// 进容器参数）。形态定位 = 真源 §5.3 的过渡形态：一次性容器（--rm 每命令
// 一弹）；终局 = §0.2/§5.3 的每会话长驻容器（Manus 形态——懒启动/会话终
// 销毁、只挂工作区、凭证在围栏外），届时本 Provider 的 Wrap 改实现为
// docker exec、容器生命周期归 Provider 内部，接口形状不变。
//
// 策略翻译（只收紧不放宽）：
//
//	Mode readonly         → --read-only + 工作区 :ro 挂载
//	Mode workspace-write  → --read-only（容器根只读）+ 工作区 :rw
//	Mode danger-full      → 容器根可写（隔离仍在——容器即边界）+ 工作区 :rw
//	Network=false         → --network none；true = 默认 bridge
//	WritableRoots         → 同路径 :rw 追加挂载（缓存类目录逃生门，与 OS 档同义）
//	ProtectedReadOnly     → 同路径 :ro 子挂载（嵌套 bind 遮蔽——docker 按路径
//	                        深度解析覆盖顺序，Landlock 做不到的回盖此处可治）
//	Env                   → -e 逐条注入；容器面天然最小环境（不继承宿主 env，
//	                        EnvMode 对本后端无操作面）
//	Limit.NProc           → --pids-limit（FileSizeMB 无 docker 等价面，不翻译）
//
// 已知缝隙（真源 §5.3 缝隙②，随终局长驻形态治）：docker run attached 形态
// 下杀 docker CLI 进程不终止容器——run_command 后台任务的 task_stop 只杀
// CLI，容器残留至自身退出（--rm 缓解一次性，长任务仍有窗口）。
//
// 降级语义（审查 A-3 明示）：daemon 不可达 → nil argv，调用方按姿态降级
// （当前接线 = auto：命令裸跑于宿主进程环境，容器隔离失效——启动告警已
// 如实声明；fail-closed 拒跑属 require 姿态接线，真源 §10.4）。
package sandbox

import (
	"log"
	"os/exec"
	"strconv"
	"sync"
)

// DockerProvider Docker 一次性容器后端（Image 空 = alpine:3.20，与既有
// dockerWrap 缺省一致）。零值可用；探测进程级缓存一次。
type DockerProvider struct {
	Image string
	// DNS/内存/挂载卷等参数位随终局长驻形态引入（YAGNI——当前无消费者）。

	once   sync.Once
	status Status
}

// Probe docker CLI/daemon 可达性探测（进程级一次缓存；daemon 不可达 =
// unusable——Wrap 返回 nil argv，调用方按姿态降级：当前接线 = auto 裸跑
// + 本告警；fail-closed 拒跑属 require 姿态，接线随首个消费者，真源 §10.4）。
func (d *DockerProvider) Probe() Status {
	d.once.Do(func() {
		if err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
			d.status = Status{
				Enforcement: EnforcementUnusable,
				Detail:      "docker daemon 不可达（CLI 缺失或未启动）: " + err.Error(),
			}
			log.Printf("einox-sandbox: Docker 后端不可用——命令将裸跑于宿主进程环境（auto 姿态降级；容器隔离失效，拒跑须 require 姿态，真源 §10.4）")
		} else {
			d.status = Status{Enforcement: EnforcementFull, Detail: "docker daemon 可达（一次性容器形态）"}
		}
	})
	return d.status
}

// Wrap 一次性容器执行参数（daemon 不可达 → nil argv，调用方按姿态降级——
// 与 OSProvider 同款 auto 裸跑语义，容器隔离失效面如实告警）。env 返回
// nil（CLI 进程继承宿主环境——DOCKER_HOST/凭证面服务于 daemon 通信，容器
// 内环境不继承宿主、只收 -e 显式注入）。
func (d *DockerProvider) Wrap(pol *Policy, workspace, cmdLine string) ([]string, []string) {
	if d.Probe().Enforcement == EnforcementUnusable {
		return nil, nil
	}
	return dockerArgv(d.image(), pol, workspace, cmdLine), nil
}

func (d *DockerProvider) image() string {
	if d.Image != "" {
		return d.Image
	}
	return "alpine:3.20"
}

// dockerArgv 一次性容器 argv 纯构造（无探测——argv 形态测试用）。工作区
// 挂载按档只发一条：docker daemon 对同一请求内重复目标 bind 直接报
// Duplicate mount point（moby daemon/volumes.go registerMountPoints——
// 后挂遮蔽仅适用于 VolumesFrom/镜像预配置来源），不能靠「先 rw 再 ro 回盖」。
func dockerArgv(image string, pol *Policy, workspace, cmdLine string) []string {
	argv := []string{"docker", "run", "--rm", "-w", "/workspace"}
	wsOpt := ""
	switch pol.Mode {
	case ModeReadOnly:
		// 全盘只读：容器根 ro + 工作区 :ro
		argv = append(argv, "--read-only")
		wsOpt = ":ro"
	case ModeWorkspaceWrite:
		// 全盘读 + 工作区写：容器根 ro，工作区默认 :rw
		argv = append(argv, "--read-only")
	case ModeDangerFullAccess:
		// 容器根可写（隔离边界仍在）；工作区默认 :rw
	}
	argv = append(argv, "-v", workspace+":/workspace"+wsOpt)
	if !pol.Network {
		argv = append(argv, "--network", "none")
	}
	// 同目标重复 bind 同样触发 Duplicate mount point（列表内重复 / 同路径
	// 横跨两列表）——去重收敛；冲突路径取 :ro（收紧方向，ProtectedReadOnly
	// 语义优先）。OS 后端对重复免疫，去重只发生在本翻译层。
	seen := map[string]bool{}
	for _, p := range pol.ProtectedReadOnly {
		if !seen[p] {
			seen[p] = true
			argv = append(argv, "-v", p+":"+p+":ro") // 嵌套 ro bind 遮蔽可写挂载
		}
	}
	for _, root := range pol.WritableRoots {
		if !seen[root] {
			seen[root] = true
			argv = append(argv, "-v", root+":"+root+":rw") // 同路径挂载：围栏内路径语义与宿主一致
		}
	}
	for _, kv := range pol.Env {
		argv = append(argv, "-e", kv)
	}
	if pol.Limit.NProc > 0 {
		argv = append(argv, "--pids-limit", strconv.Itoa(pol.Limit.NProc))
	}
	argv = append(argv, image, "sh", "-c", cmdLine)
	return argv
}
