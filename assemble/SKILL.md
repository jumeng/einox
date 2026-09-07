---
name: einox-assemble
description: 在 einox 基座上开发业务 agent 时使用——逐能力确认选型、产出 einox.agent.yaml 装配清单、按规则与套路生成 agent 内核。触发词:开发 agent、新建 agent、装配 einox、生成 agent 内核、einox-assemble。
---

# einox-assemble:装配引导与内核生成

把 einox 能力面组装成业务 agent 内核。你是执行者:人选能力(清单是唯一契约),你按规则与套路写代码。本目录即完整知识层,参考文件按相对路径引用。

## 六步流程

### ① 场景定位

问用户业务场景,映射推荐 recipe;用户直接指定则跳过。呈现所选 recipe 的能力组合表([recipes/](recipes/) 对应篇),说明这是打底基线、接下来逐域确认增删。

### ② 逐域确认

按 [rules.md](rules.md) 选型域序逐项过:

```
① 模型面 → ② 引擎域 → ③ 审批域 → ④ 工具面 → ⑤ harness 域
→ ⑥ 质量域 → ⑦ 安全面 → ⑧ 渠道 → ⑨ 界面
```

- 每项呈现:**能力名 + 一句话说明 + 对业务的影响**。
- 用户询问详情时展开该项语义(rules.md 档位语义表;更深处引 einox 仓 docs/03 对应行)。
- 逐项三选一:**加入**(定参数)/ **跳过**(用 recipe 基线)/ **排除**(`false` 落清单)。
- 基线已含且用户无异议的项快速通过,不逐项复读——重点问基线未覆盖与业务强相关的项。

### ③ 清单落盘

把确认结果写成业务仓根 `einox.agent.yaml`(格式:[manifest-spec.md](manifest-spec.md);preset 打底 + 显式项覆盖)。**落盘前完整呈现给用户过目确认**。

### ④ 生成内核

按 [patterns/](patterns/) 拼装:

1. [patterns/00-skeleton.md](patterns/00-skeleton.md) 为底(四必填 + 演示级 Store);
2. 清单每个启用项叠加对应 patterns 段(逐域文件,段自足可拼装);
3. Instruction 按拼装序:业务职责段(与用户协作产出)+ `prompts.Coding()`(fs/cmd/patch 未全裁)+ `prompts.Orchestration()`(subagents)+ 会话配置段;
4. 产出形态二选一:
   - **绿地模式**(空业务仓):main.go + filestore.go + go.mod + README.md(装配说明与运行方法);
   - **嵌入既有仓模式**(业务仓已有代码/底座/纪律):遵循该仓 AGENTS.md 约定——复用其既有 Store/打印器等共享底座、示例目录形态与测试纪律;清单落仓根,装配代码形态向仓内惯例对齐(skeleton 的 filestore.go 在已有 Store 实现时不复制);
5. 装配中逐项对照 [rules.md](rules.md) 依赖/互斥律自查——**违律即停,回用户改清单**。

### ⑤ 验收门

- `go build ./...` 通过(不过即修,修不动如实报错)。
- 输出**对账表**:清单项 → 装配代码位置,双向可溯源(每个装配段可溯源到清单项,清单每个启用项都有装配段)。不要求零发挥,要求发挥可对账。

### ⑥ 增量模式

业务仓已有 `einox.agent.yaml` 时自动进入:

1. diff 新旧清单,列出变更项;
2. 只动涉及面的代码,不动未变更能力对应的装配段;
3. 违律变更(如新启用 recall 但 fs 族被裁)按 rules 回退询问。

## 参考文件

- [manifest-spec.md](manifest-spec.md) 清单格式(九域字段规范)
- [rules.md](rules.md) 装配规则(必选/依赖/互斥/档位)+ 选型域序
- [recipes/](recipes/) 预设组合(minimal / coding / support / data-analysis)
- [patterns/](patterns/) 装配套路(00-skeleton + 九域样板)
- einox 仓 docs/03(能力全量清单)与 docs/04(装配面)——本目录复制出仓后不随行,需深处语义时读 einox 仓内文件

## 获取与安装(无需克隆 einox 仓)

知识层随 Go module 分发——业务仓 `require github.com/jumeng/einox` 后,本地 module cache 即含全部知识层文件。定位:

```bash
go list -m -f '{{.Dir}}' github.com/jumeng/einox   # → <dir>,知识层在 <dir>/assemble/
```

(知识层版本 = go.mod require 的版本,与基座代码严格同版,不存在文档漂移;升级基座后 `go mod tidy` 重新定位即得新版知识层。)

两种消费方式:

1. **零安装(AI 自主装配)**——不装 skill,AI 编码代理直接读知识层按本文件流程执行。推荐在业务仓 `AGENTS.md` 贴入一行,代理常驻知晓:

   ```markdown
   本仓基于 einox 开发 agent。装配走 einox-assemble 知识层:经
   `go list -m -f '{{.Dir}}' github.com/jumeng/einox` 定位 einox 后,
   读 `<dir>/assemble/SKILL.md` 并按其流程执行。
   ```

2. **skill 常驻(引导式逐项确认)**——从 module cache 复制安装:

   ```bash
   cp -r "$(go list -m -f '{{.Dir}}' github.com/jumeng/einox)/assemble" \
         ~/.agents/skills/einox-assemble
   ```

   目录自含(参考文件全部相对路径引用);升级基座后重跑此命令。
