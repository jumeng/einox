package llm

// 预置供应商目录与引用策略（2026-08-28 定案：厂家知识住基座）。
//
// 定位演进：2026-08-21 定案 BuiltinProviders 仅作模型页「厂家」下拉模板、
// 不参与运行时；2026-08-28 供应商适配架构定案升格为**引用策略**——业务层
// 选择引用方式（自定义 = Resolve/ResolveFile 原样；都引用 = ResolveMerged/
// ResolveFileMerged，本产品取都引用档）。
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
// DeepSeek 官方两协议端点：openai 兼容（api.deepseek.com，主推格式，
// dialect=deepseek 思考字段）+ anthropic 兼容（/anthropic，思考走协议原生
// 预算档零方言）；模型三只（flash/pro/vision-exp，官方定价页：上下文均
// 1M、最大输出 384K，vision-exp 定价与 flash 一致），1M 预开。
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
			ID: "deepseek-anthropic", Name: "DeepSeek（Claude 协议）", Kind: "anthropic",
			BaseURL: "https://api.deepseek.com/anthropic", Enabled: true,
			Models: []ModelSpec{ // 思考走 anthropic 协议原生预算档（ThinkingConfig），零方言
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

// MergeBuiltin 引用策略「都引用」合并原语：用户条目 by-ID 匹配内置预设，
// 缺省参数补全（见包注释语义）。返回新切片；无匹配条目原样透传。
func MergeBuiltin(ps []ProviderSpec) []ProviderSpec {
	builtin := map[string]ProviderSpec{}
	for _, b := range BuiltinProviders() {
		builtin[b.ID] = b
	}
	out := make([]ProviderSpec, 0, len(ps))
	for _, p := range ps {
		b, ok := builtin[p.ID]
		if ok {
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

// ResolveFileMerged 文件层 + 内置合并（选择器/校验/引擎同世界观的读侧视图；
// 合并在归一前——Kind 空位须留给内置补全，先归一会误填 anthropic）。
func ResolveFileMerged(st Store) []ProviderSpec {
	return NormalizeProviders(MergeBuiltin(resolveFileRaw(st)))
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
