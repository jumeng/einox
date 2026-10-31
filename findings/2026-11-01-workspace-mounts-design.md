# 设计件:工作区挂载区写放行(WorkspaceMounts)

日期:2026-11-01 ｜ 触发:P16 Gen-UI 投影批段四(模型经 `<ws>/visualize/` 挂载
写可视化片段被 `tools.ResolveUnder` 符号链接圈禁拒);同缝翻出 P15 潜在缺口
(项目会话经 `<ws>/project/` 挂载的模型写同样被拒——P15 验证覆盖建会话/读
路径,未覆盖项目会话写轮)。

## 问题

`tools.ResolveUnder`(tools/resolve.go,审查 P1-5 单点)对「区内指向区外的
symlink」一律拒绝——写面逃逸闭合,语义正确。但应用层的工作区挂载区
(`<ws>/project` → 项目目录、`<ws>/visualize` → 可视化目录)是**宿主自建、
有意的越区挂载**:链接目标由应用受控创建(engine 之外),模型写经挂载点进
入目标是产品语义(项目编辑/可视化产图),非逃逸。当前实现把两者混为一谈,
挂载区写全拒。

## 契约(最小缝)

1. `tools.ResolveUnderMounts(root, p string, mounts []string) (string, error)`:
   - `mounts` 为顶层挂载名清单(调用方经 `tools.CheckTopDirs` 校验;engine
     Options 层已校验)。
   - `p` 首段 ∈ mounts:词法圈禁改为对**挂载点之后的剩余路径**(禁 `..`/绝
     对);符号链接圈禁改为**相对挂载点真身**——挂载点本身允许越区(宿主
     受控),挂载目标内部的进一步越区链接仍拒(fail-closed 不变)。
   - 返回值为**词法路径**(`<root>/<mount>/<rest>`,不解析链接——与
     ResolveUnder 同形,打开语义由各写面既有保护叠加)。
   - `ResolveUnder(root, p)` ≡ `ResolveUnderMounts(root, p, nil)`:既有行
     为与全部既有调用零变化。
2. 写面构造器增可选 `Mounts []string`(nil=零变化):`applypatch`(补丁写)、
   `fsutil`(write/edit/delete 族)。office/runcommand/extwire 暂不接(无挂
   载消费面;缝已定型,需要时同形接入)。
3. `engine.Options.WorkspaceMounts []string`(CheckTopDirs 校验):assemble
   期传给 applypatch/fsutil。与 WorkspaceKeep/WorkspaceProtect 同族同形。

## 安全边界(不变量)

- 挂载区外的路径:行为与现状逐字节一致(既有测试为回归底册)。
- 挂载区内的越区链接(挂载目标内部再挂出):仍拒。
- `..`/绝对路径/多段穿越:仍拒(对剩余路径词法圈禁)。
- 挂载区清单是**应用装配声明**,模型不可感知/不可扩(不进工具描述)。

## 测试

- base:resolve 挂载三态(挂载区写放行/区内越区链接拒/`..` 拒)+ 既有
  ResolveUnder 测试零变化;applypatch/fsutil 经挂载写往返。
- app(einox-work):assembly `WorkspaceMounts: ["project", "visualize"]`;
  smoke --genui 断言片段经挂载落真源;P15 项目写缺口随缝闭合(登记
  FEATURE-MAP)。

## 备选与弃用理由

- 应用层改真目录(非挂载):破坏 Gen-UI 固定输出根契约(跨会话读/fork 沿
  用),弃。
- ResolveUnder 全局放行 WorkspaceKeep 名单:Keep≠Mount 语义(Keep 是清理
  保留,不构成写放行依据),弃。
