# 05 · 沙箱

> 沙箱是 `run_command` 的 per-execution 策略围栏——命令能读什么、写哪里、能否联网、耗多少资源，由内核强制。它是纵深防御中的一层，不替代 HITL 审批与出口治理，也不替代容器边界（容器保护宿主机，不保护同容器、同挂载卷的数据与进程）。`Options.Sandbox` 为 nil 即不沙箱（默认关）；启用前提与平台限制见本文。借鉴的开源来源与协议逐项登记于 [NOTICE](../NOTICE.md)。

## 策略模型（sandbox.Policy）

| 字段 | 说明 |
|---|---|
| `Mode` | 三档：`read-only`（全盘只读）/ `workspace-write`（全盘读 + 工作区与临时目录写，默认档）/ `danger-full-access`（不加文件围栏，仅资源限额与可选断网生效） |
| `Network` | 默认 `false` 断网；LLM 调用发生在服务进程内，不受围栏影响 |
| `WritableRoots` | `workspace-write` 档追加可写根——HOME 下的缓存目录（go build / npm 等）必须落在此处，否则依赖安装必败且模型无法自纠 |
| `ProtectedReadOnly` | 可写区内希望保持只读的子路径（如 `.git`）。平台能力差异大，见“平台已知限制” |
| `Env` | 附加环境变量（`K=V`），缓存重定向的载体（`GOCACHE` / `GOMODCACHE` / `npm_config_cache` 指向可写根） |
| `EnvMode` | 环境继承档：缺省 `inherit`（全继承，但剔除 `LLM_*` 凭证面与策略载荷）/ `minimal`（白名单：`PATH` / `HOME` / `TMPDIR`，Windows 另含 `TEMP` / `SystemRoot` 等基础键——其余环境一律经 `Env` 显式注入） |
| `ExcludeTmpdir` / `ExcludeSlashTmp` | 默认 `false`：`$TMPDIR` 与 `/tmp` 计入可写根；置 true 显式排除 |
| `Limit` | 资源限额：`NProc` 默认 512、`FileSizeMB` 默认 1024、core dump 恒 0。跳过 `RLIMIT_AS`——Go 工具链运行时与链接器有 GB 级虚拟地址预留，内存硬限额误杀合法构建，归 cgroup / 容器层 |

模式名在构造期 `Validate()` fail-fast，未知值不进运行期。`EnvMode` 的取舍是结构性的：`inherit` 是拒绝清单——记住要剥谁，对未知凭据名结构性失效；`minimal` 是允许清单——业务所需环境一律显式注入。多租户或含密钥环境建议 `minimal`。

## 后端矩阵

| 后端 | 平台 / 形态 | 说明 |
|---|---|---|
| `sandbox.OSProvider`（默认） | Linux x86_64 / arm64 | Landlock 文件围栏 + seccomp 经典 BPF 断网（仅断网档安装；default allow + deny 清单，connect 无条件拒绝、AF_UNIX 同断——防 docker.sock 类本地套接字逃逸）+ rlimit |
| | macOS | Seatbelt（`/usr/bin/sandbox-exec` 固定路径，不查 PATH） |
| | Windows | restricted token（剥特权 + 可写根 ACL 授权） |
| | 其余 Linux 架构 | 运行时桩如实报 `unusable`（seccomp 号表仅 x86_64 / aarch64）——不做编译期拦截，保证仓库在任意平台可构建 |
| `sandbox.DockerProvider` | 任意平台，一次性容器（`--rm`） | 策略翻译为容器参数：`read-only` → `--read-only` + 工作区 `:ro`；`workspace-write` → 容器根 `--read-only` + 工作区 `:rw`；断网 → `--network none`；`WritableRoots` 同路径 `:rw` 挂载；`ProtectedReadOnly` 以嵌套 `:ro` bind 遮蔽（Landlock 做不到的回盖此处可治）；`NProc` → `--pids-limit`（`FileSizeMB` 无等价面不翻译）。默认镜像 `alpine:3.20`，需工具链请自备镜像。**daemon 不可达 = 容器隔离失效**：按姿态降级（当前接线 auto——命令裸跑于宿主进程环境 + 启动告警；需要 fail-closed 拒跑的部署待 require 姿态接线） |
| 自定义后端 | 经 `engine.Options.SandboxProvider` 注入 | gVisor / 微 VM 等按 `sandbox.Provider` 接口实现（两个方法：`Wrap` 构造执行参数、`Probe` 上报状态） |

`Provider` 契约：**Probe 必须如实上报**（执行力三态，见下节）；**只许收紧不许放宽**——翻译结果宽于策略即实现违约；本次不可沙箱时返回 nil argv，由调用方按后端姿态降级。已知缝隙：Docker 一次性形态下杀 docker CLI 进程不终止容器（`--rm` 缓解，长任务仍有残留窗口），随每会话长驻容器形态治理。

## re-exec 哨兵协议

Landlock / seccomp / rlimit 必须在 fork 后、exec 前于子进程内施加，Go 的惯例是自身 re-exec：run_command 以 `[<exe>, "__einox-sandbox", "--", "sh", "-c", <cmd>]` 重新执行宿主程序自身，宿主 `main()` 顶部挂一行 `sandbox.RunHelper(args)` 拦截哨兵子命令——helper 路径内按序施加（`LockOSThread` → `NO_NEW_PRIVS` → seccomp → Landlock → rlimit → `execve` 真实命令）。策略 JSON 经环境变量传递，不污染 `ps` 输出。

挂钩是本机制对宿主程序的唯一侵入点（一行）。未挂钩时探测报 `unusable` 而非静默裸跑。

**探测序**（进程级缓存一次，装配期调用即可让告警进启动日志）：

1. 哨兵握手——证明 `main` 已挂 `RunHelper`（内核 ABI 探测只证内核支持，不证哨兵在位）；
2. 后端实测——re-exec 子进程**自施加策略后**在围栏内跑写探针再上报，不做“--version 式”纸面检查（会漏有 syscall 但拒执行的内核）。

应答约定行 `einox-sandbox-enforce full|partial abi=N [uncovered=…]`，运维核查启动日志即见。

## Enforcement 三态与后端姿态

执行力如实三态上报：

- **full**：目标策略全量生效；
- **partial**：生效但有未覆盖项，`Uncovered` 逐项列出（如 Linux 旧内核 ABI 缺位：ABI<2 缺 `refer`、<3 缺 `truncate`、<5 缺 `ioctl-dev`——不在受控位集的访问即不受围栏，上报口径偏保守）；
- **unusable**：后端不可用（未挂钩 / 内核不支持 / 平台未实现），`Detail` 给出诊断。

探测不可用时的处置由 `Backend` 姿态决定：`off`（nil 即此态，默认不沙箱）/ `auto`（不可用则裸跑 + 启动告警——不允许隐式 fail-closed）/ `require`（不可用即拒跑，fail-closed）。

## 部署前提

| 项 | 要求 |
|---|---|
| 宿主 `main` | 顶部挂 `sandbox.RunHelper(os.Args)` 一行（哨兵协议的挂钩点） |
| Linux 内核 | ≥ 5.13（Landlock 合入版本）**且内核编入 Landlock**——版本号够但 `CONFIG_SECURITY_LANDLOCK` 未开同样不可用；容器共享宿主内核，这是内核能力不是镜像能力 |
| Linux 容器内 | 运行时默认 seccomp profile 须放行 `landlock_*` 三个 syscall（新版 Docker 已含；受阻则自定义 profile） |
| Docker 后端 | docker CLI 与 daemon 可达；离线分发随包带镜像 |
| macOS | 无额外前提；`sandbox-exec` 名义 deprecated 但无替代品（系统自用） |

装配期行为：`Options.Sandbox` 非 nil 且 cmd 族未被 `SessionToolsOff` 裁剪时，engine 构造期自动 `Probe()` 一次，后端状态进启动日志。装配示例见 [04-assembly.md](04-assembly.md)“沙箱装配”。

## 平台已知限制

**Linux**：seccomp 只在断网档（`Network=false`）安装——放行网络即不装 seccomp，ptrace / io_uring 免杀清单随之失效，该面归部署层；`ProtectedReadOnly` 不可实现——Landlock 规则只能授不能收，可写区内只读回盖做不到，探测报 partial；`NProc` 对 root 进程被内核豁免（容器 root 形态无效，进程数硬约束归 cgroup `pids-max`）；触顶报 `EAGAIN` 不命中拒绝签名，模型看到的是莫名失败——大产物场景经 `Limit` 调大。

**Windows**：网络禁断不支持（WFP 需管理员安装持久过滤，后置）——Enforcement 恒 partial、`uncovered=network`；无进程组语义，超时 / 停止退化为单进程终结，孙进程残留但继承 restricted token、围栏不破（存活性缺口而非逃逸）；进程级硬约束归 Job Object（未做）。`ProtectedReadOnly` 经 deny ACE 真回盖。

**macOS**：`ProtectedReadOnly` 经 require-not 排除真回盖（含保护路径祖先目录的改名防护、嵌套 symlink 组件拒绝——跟随解析会把路径检查变成新的授权授予）；偏好读取类工具在围栏内可能报错（平台默认放行项未纳入 profile）。

**通用残留面**：信号未治理——seccomp deny 清单不含 kill（拒绝 kill 会误杀围栏内合法的作业控制），同 uid 的组外进程（含宿主服务自身）仍可被信号触达，硬约束归容器 / cgroup 层；全盘只读档授予面仍是“服务可读即沙箱可读”，机密性防护归审批层，非 OS 围栏承诺。

**验证口径**：Linux 后端可全实测（单测覆盖围栏语义、断网、限额、崩溃恢复）；darwin / windows 后端为编译级 + 纯逻辑单测（Seatbelt profile 文本断言 / SID 派生断言），运行时行为待对应平台宿主验证——以探测三态的实报为准，不做过能力承诺。

## 环境面治理

围栏内环境继承分两档（`EnvMode`）：缺省 `inherit` 全继承，但强制剔除 `LLM_*` 凭证面与哨兵策略载荷——密钥不随沙箱命令下传是硬规则；`minimal` 白名单档只保留基础键，业务所需环境一律经 `Policy.Env` 显式注入。注意 `minimal` 治理的是 **env 面**凭据（`AWS_*` / `GITHUB_TOKEN` 类环境变量）；`~/.aws` 等文件面凭据的可读性仍由 `Mode` 档决定（readonly 档全盘可读是模式语义，机密性归审批层）。缓存重定向是 `workspace-write` 档的标配惯例：围栏内 HOME 不可写，`GOCACHE` / `GOMODCACHE` / `npm_config_cache` 必须指向 `WritableRoots`，否则构建工具链硬失败且无回退。

## 拒绝反馈

每个后端声明自己的拒绝签名：Landlock `permission denied` / seccomp `operation not permitted` / Seatbelt `operation not permitted` / Windows `access is denied`。命中即由 `sandbox.DenialHint` 在错误信封附提示行——文案是模型友好的硬边界声明：重试或换工具无效，确需该操作用 `ask_user` 向用户说明、由用户调整沙箱配置或人工执行。不提供“走审批加宽”通道（围栏语义与审批矩阵解耦）。

## 与审批、出口治理的关系

- **沙箱不替代审批**，是审批粒度的杠杆：无沙箱时每条写命令都需人批（审批疲劳会让防线事实失效）；启用后围栏内操作可放行、越界操作升级人工——是否据此调整审批矩阵属应用决策。
- **出口治理（egress）是正交的另一层**：沙箱断网是内核层全有或全无，`Network=true` 时内核层网络治理为零——`tools/egress` 的 URL 前置校验（私网默认阻断 + CIDR 白名单 + DNS pinning）补位，覆盖 `web_fetch` 与 `run_command` 命令串预检，见 [03-capabilities.md](03-capabilities.md)。
- 部署形态与边界层级的选择（容器 / OS 沙箱 / 审批 / 工作区圈限如何组合纵深）见 [03-capabilities.md](03-capabilities.md)“隔离边界随部署形态选择”。
