# 安全面样板(security)

> 清单域 ⑦:`sandbox.*` / `egress`。egress 装配写法见 [tools.md](tools.md)——本篇讲沙箱装配与部署形态边界选择。沙箱、出口治理、HITL 审批 + 工作区圈限各层独立生效,组合成纵深。

## sandbox(命令沙箱)

```go
Sandbox: &sandbox.Policy{
    Mode:          sandbox.ModeWorkspaceWrite, // ModeReadOnly / ModeWorkspaceWrite / ModeDangerFullAccess
    Network:       true,   // 断网档下依赖安装必死且模型无法自纠——内网/需拉依赖形态须配 on
    EnvMode:       sandbox.EnvMinimal,         // 环境白名单——凭据面默认不进围栏(缺省 inherit 全继承)
    WritableRoots: []string{cacheDir},          // 围栏内 HOME 不可写——缓存必须落在可写根
    Env: []string{                              // 缓存重定向(不重定向 go build 硬失败无回退)
        "GOCACHE=" + cacheDir + "/go-build",
        "GOMODCACHE=" + cacheDir + "/go-mod",
        "npm_config_cache=" + cacheDir + "/npm",
    },
},
```

**main 顶部必挂哨兵钩子**(OS 后端 = re-exec 哨兵协议;漏挂 = 沙箱不可用仅启动告警,不崩):

```go
func main() {
    sandbox.RunHelper(os.Args) // 命中 __einox-sandbox 哨兵子命令进入 helper 路径,未命中原样返回
    // ……正常装配流程
}
```

容器后端(无哨兵依赖,策略翻译进容器参数):

```go
SandboxProvider: &sandbox.DockerProvider{Image: "golang:1.26"},
```

平台机制与部署前提见 einox 仓 docs/05-sandbox.md:Linux Landlock+seccomp(amd64/arm64)、Windows restricted token、macOS Seatbelt;`ProtectedReadOnly` 经嵌套 ro bind;daemon 不可达 = 按姿态降级裸跑 + 启动告警。

## 部署形态边界选择(隔离边界在哪一层是部署决策)

| 部署形态 | 边界选择 |
|---|---|
| 服务器形态(B/S、多用户) | 服务整体容器化 = 第一层粗粒度边界;应用面靠审批矩阵 + 会话工作区圈限;OS 沙箱选配开启 = 第二层纵深 |
| 终端形态(CLI 跑用户本机、单用户) | 无容器边界,命令直接落宿主 OS——Landlock/seccomp / Seatbelt / restricted token 就是主执行边界 |
| 学习 / 研究 | 容器即边界,内层沙箱可省;默认容器共享内核非强安全边界——保持非特权、裁剪能力集;强隔离再上 gVisor / 微 VM |

## egress(交叉引用)

装配写法与语义见 [tools.md](tools.md) egress 节;与沙箱正交(用户态出口治理):私网默认阻断 + CIDR 白名单即工作面,`web_fetch` 前置与 `run_command` 命令串预检共用同一校验器。

## 验证

- `go build ./...` 过。
- 沙箱实测:run_command 形态命令在 read-only 围栏内写文件 → 拒;Policy.Mode 非法值 → `Validate()` 构造期拒。
