package llm

// 预置供应商目录与引用策略（2026-08-28 定案：厂家知识住基座）。
//
// 定位演进：2026-08-21 定案 BuiltinProviders 仅作模型页「厂家」下拉模板、
// 不参与运行时；2026-08-28 供应商适配架构定案升格为**引用策略**——业务层
// 选择引用方式（自定义 = Resolve/ResolveFile 原样；都引用 = ResolveMerged/
// ResolveFileMerged，本产品取都引用档）；2026-09-02 应用层预置定案——
// 业务特有厂家不进基座目录，住应用仓经注入面与内置同构参与合并，且
// **应用对基座标准目录有挑选权**（呈现 = 应用挑选的基座子集 + 应用私有
// 全集，经 ResolveFileCatalog/ResolveCatalog 自备完整目录落地；基座只有
// 能力套件：目录合并管道与规格改写工厂缝〔model.go RewriteSpec〕；厂家
// 特有协议与参数适配一律归应用层）。
//
// 合并语义（MergeBuiltin，by-ID 参数补全）：
//   - 用户条目按 ID 匹配内置条目，**缺省字段补全**：Kind/BaseURL/Name/
//     Dialect/Models（用户显式值优先——含 BaseURL 覆写走代理）
//   - APIKey/Enabled/Catalog 永不受内置影响（密钥只属用户层；启用权归用户）
//   - 无用户匹配的内置条目保持休眠（不注入清单）——旧不变量「空实例配置
//     = 系统无可用模型」天然保住：内置永不独立出现，只补全已配置条目
//   - 合并返回新切片，不改入参（读侧视图，绝不回写存储——settings 回显
//     走原样盘，合并只发生在 resolve 视图）
//
// DeepSeek 官方推荐 openai 协议端点（api.deepseek.com，dialect=deepseek
// 思考字段）；模型三只（flash/pro/vision-exp，官方定价页：上下文均 1M、
// 最大输出 384K，vision-exp 定价与 flash 一致），1M 预开。/anthropic 兼容
// 端点不预置（2026-09-01 裁撤内置条目，只留官方推荐接入口）——需要时
// 自定义 Kind=anthropic 接，思考走协议原生预算档零方言。
//
// 智谱（BigModel）单条目：GLM-5.3（纯文本）+ GLM-5.3-Flash（原生多模态，
// 图片经 image_url 传 URL/Base64），上下文 1M、最大输出 128K（两模型文档
// 同口径）；Chat Completion 端点 + dialect=glm（开档 thinking enabled +
// reasoning_effort 恰 low/high/max 三值直传、关档 thinking disabled——与
// deepseek 方言线格式同形；GLM-5.3/5.3-Flash 文档限制只能开启，关档线格式
// 照发由端点表态）。
// 本预置面向智谱开放平台 API（API 密钥计费面），接入面即 openai 兼容
// Chat Completion（paas/v4）；anthropic 兼容端点属 GLM Coding Plan 接入
// 面、非本密钥可用，不预置。采样参数不设——GLM-5.x 端点默认值即文档
// 推荐值（temperature 1.0 / top_p 0.95，且二选一）。

// BuiltinProviders 预置供应商目录（纯数据、版本化随基座、不含密钥；模型页
// 「厂家」下拉加载模板同源）。
func BuiltinProviders() []ProviderSpec {
	m1m := func(id string, image bool, pri int) ModelSpec {
		m := ModelSpec{
			ID: id, Input: []string{"text"},
			Limit:    &Limit{Context: 1_000_000, Output: 128_000}, // 1M 上下文预开
			Priority: pri,
		}
		if image {
			m.Input = []string{"text", "image"}
		}
		return m
	}
	return []ProviderSpec{
		{
			ID: "deepseek", Name: "DeepSeek", Kind: "openai",
			BaseURL: "https://api.deepseek.com", Dialect: "deepseek", Enabled: true,
			Models: []ModelSpec{
				m1m("deepseek-v4-flash", false, 100),
				m1m("deepseek-v4-pro", false, 101),
				m1m("deepseek-v4-flash-vision-exp", true, 102),
			},
		},
		{
			ID: "zhipu", Name: "智谱（GLM）", Kind: "openai",
			BaseURL: "https://open.bigmodel.cn/api/paas/v4", Dialect: "glm", Enabled: true,
			Models: []ModelSpec{
				m1m("glm-5.3-flash", true, 103), // 原生多模态：图片输入
				m1m("glm-5.3", false, 104),      // 纯文本
			},
		},
	}
}

// MergeProviders 任意预置目录的 by-ID 合并原语（2026-09-02 应用层预置
// 定案配套能力：内置目录 + 应用仓扩展目录同构参与——provider 级补全
// 语义同 MergeBuiltin：缺省字段填充、用户显式值优先、无匹配条目原样
// 透传、不独立注入）。
func MergeProviders(catalog, ps []ProviderSpec) []ProviderSpec {
	builtin := map[string]ProviderSpec{}
	for _, b := range catalog {
		builtin[b.ID] = b
	}
	out := make([]ProviderSpec, 0, len(ps))
	for _, p := range ps {
		if b, ok := builtin[p.ID]; ok {
			if p.Kind == "" {
				p.Kind = b.Kind
			}
			if p.BaseURL == "" {
				p.BaseURL = b.BaseURL
			}
			if p.Name == "" {
				p.Name = b.Name
			}
			if p.Dialect == "" {
				p.Dialect = b.Dialect
			}
			if len(p.Models) == 0 {
				p.Models = b.Models
			}
		}
		out = append(out, p)
	}
	return out
}

// MergeBuiltin 引用策略「都引用」合并原语：用户条目 by-ID 匹配内置预设，
// 缺省参数补全（见包注释语义）。返回新切片；无匹配条目原样透传。
func MergeBuiltin(ps []ProviderSpec) []ProviderSpec {
	return MergeProviders(BuiltinProviders(), ps)
}

// ResolveFileMerged 文件层 + 内置合并（选择器/校验/引擎同世界观的读侧视图；
// 合并在归一前——Kind 空位须留给内置补全，先归一会误填 anthropic）。
func ResolveFileMerged(st Store) []ProviderSpec {
	return NormalizeProviders(MergeBuiltin(resolveFileRaw(st)))
}

// ResolveFileCatalog 文件层 + 应用自备目录合并（2026-09-02 应用挑选权
// 定案配套能力：目录 = 应用挑选的基座标准供应商子集 + 应用私有全集，由
// 应用层全权决定、展示与运行时同源；不在目录内的条目即使内置有预设也
// 不被补全——排除语义在此落地。文件层读盘〔含 v1 识别迁移〕与合并次序
// 〔归一前，Kind 空位留给目录补全〕同 ResolveFileMerged）。
func ResolveFileCatalog(st Store, catalog []ProviderSpec) []ProviderSpec {
	return NormalizeProviders(MergeProviders(catalog, resolveFileRaw(st)))
}

// ResolveFileMergedWith 文件层 + 内置 + 应用层扩展预置合并（应用预置注入
// 面的便捷档——目录 = 全量内置 + extra；要挑选基座子集用 ResolveFileCatalog
// 自备完整目录）。extra 条目与内置同构：by-ID 匹配已配置条目做补全，
// 不独立注入。
func ResolveFileMergedWith(st Store, extra []ProviderSpec) []ProviderSpec {
	catalog := BuiltinProviders()
	if len(extra) > 0 {
		catalog = append(catalog, extra...)
	}
	return ResolveFileCatalog(st, catalog)
}

// ResolveMerged 运行时 resolve 都引用档（env > 文件层合并 > yaml 手动层；
// env/yaml 逃生层不合并——显式配置原样，与自定义档同形）。
func ResolveMerged(st Store) []ProviderSpec {
	if p, ok := envProvider(); ok {
		return []ProviderSpec{p}
	}
	if ps := ResolveFileMerged(st); len(ps) > 0 {
		return ps
	}
	return resolveYAML()
}

// ResolveCatalog 运行时 resolve 的应用自备目录版（语义同 ResolveMerged，
// 文件层换 ResolveFileCatalog）。
func ResolveCatalog(st Store, catalog []ProviderSpec) []ProviderSpec {
	if p, ok := envProvider(); ok {
		return []ProviderSpec{p}
	}
	if ps := ResolveFileCatalog(st, catalog); len(ps) > 0 {
		return ps
	}
	return resolveYAML()
}

// ResolveMergedWith 运行时 resolve 都引用档的应用预置扩展版（语义同
// ResolveMerged，文件层换 ResolveFileMergedWith）。
func ResolveMergedWith(st Store, extra []ProviderSpec) []ProviderSpec {
	if p, ok := envProvider(); ok {
		return []ProviderSpec{p}
	}
	if ps := ResolveFileMergedWith(st, extra); len(ps) > 0 {
		return ps
	}
	return resolveYAML()
}
