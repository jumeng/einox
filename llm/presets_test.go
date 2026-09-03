package llm

// 预置引用策略回归（2026-08-28 定案）：MergeBuiltin by-ID 参数补全语义
// （用户显式值优先/密钥与启用权不受内置影响/休眠不注入/入参不改写）+
// ResolveFileMerged 世界观 + 通用思考方言 effortThinkingFields + 校验值域。

import (
	"context"
	"strings"
	"testing"
)

// memStore 内存 Store 桩（llm-settings.json 单文件面）。
type memStore struct{ data []byte }

func (m *memStore) ReadLLMFile(string) ([]byte, bool) {
	if m.data == nil {
		return nil, false
	}
	return m.data, true
}

func TestMergeBuiltinSemantics(t *testing.T) {
	user := []ProviderSpec{
		{ // 命中内置：仅密钥+启用——参数全靠内置补全
			ID: "deepseek", APIKey: "sk-x", Enabled: true,
		},
		{ // 命中内置：显式值优先（BaseURL 覆写走代理 + 自裁模型清单——合并语义
			// 单测按条目各自合并，与首条同 ID 不冲突；ID 唯一性归 ValidateProviders）
			ID: "deepseek", BaseURL: "https://proxy.internal/deepseek",
			APIKey: "sk-y", Enabled: true,
			Models: []ModelSpec{{ID: "deepseek-v4-pro", Input: []string{"text"}}},
		},
		{ // 未命中：原样透传
			ID: "vllm", Kind: "openai", BaseURL: "http://10.0.0.2:8000/v1",
			APIKey: "k", Enabled: true,
			Models: []ModelSpec{{ID: "acme-7b", Input: []string{"text"}}},
		},
		{ // 命中内置：智谱极简引用——参数全靠内置补全（glm 方言 + 多模态标记）
			ID: "zhipu", APIKey: "zp-x", Enabled: true,
		},
	}
	got := MergeBuiltin(user)

	if len(got) != 4 {
		t.Fatalf("合并不得增删条目：%d", len(got))
	}
	d := got[0]
	if d.Kind != "openai" || d.BaseURL != "https://api.deepseek.com" || d.Dialect != "deepseek" || d.Name != "DeepSeek" {
		t.Fatalf("内置参数应补全：kind=%s base=%s dialect=%s name=%s", d.Kind, d.BaseURL, d.Dialect, d.Name)
	}
	if d.APIKey != "sk-x" || !d.Enabled {
		t.Fatalf("密钥/启用权不得受内置影响：%+v", d)
	}
	if len(d.Models) != 3 || d.Models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("空模型清单应取内置全套：%+v", d.Models)
	}
	if d.Models[2].Input == nil || !SupportsImage(d.Models[2]) {
		t.Fatalf("vision 模型能力标记应随内置带出")
	}

	a := got[1]
	if a.BaseURL != "https://proxy.internal/deepseek" {
		t.Fatalf("用户 BaseURL 显式值应优先：%s", a.BaseURL)
	}
	if a.Dialect != "deepseek" || a.Kind != "openai" {
		t.Fatalf("显式清单条目的空位仍应内置补全：kind=%s dialect=%s", a.Kind, a.Dialect)
	}
	if len(a.Models) != 1 || a.Models[0].ID != "deepseek-v4-pro" {
		t.Fatalf("用户显式模型清单应保留（自裁语义）：%+v", a.Models)
	}

	v := got[2]
	if v.BaseURL != "http://10.0.0.2:8000/v1" || len(v.Models) != 1 || v.Models[0].ID != "acme-7b" {
		t.Fatalf("未命中条目应原样：%+v", v)
	}

	z := got[3]
	if z.Kind != "openai" || z.BaseURL != "https://open.bigmodel.cn/api/paas/v4" || z.Dialect != "glm" || z.Name != "智谱（GLM）" {
		t.Fatalf("智谱内置参数应补全：kind=%s base=%s dialect=%s name=%s", z.Kind, z.BaseURL, z.Dialect, z.Name)
	}
	if len(z.Models) != 2 || z.Models[0].ID != "glm-5.3-flash" || !SupportsImage(z.Models[0]) {
		t.Fatalf("flash 应为首模型且带图片输入能力：%+v", z.Models)
	}
	if z.Models[1].ID != "glm-5.3" || SupportsImage(z.Models[1]) {
		t.Fatalf("glm-5.3 应为纯文本模型：%+v", z.Models[1])
	}

	// 入参不改写（读侧视图纪律）
	if user[0].Kind != "" || len(user[0].Models) != 0 {
		t.Fatalf("MergeBuiltin 不得改写入参")
	}

	// 休眠不注入：空清单合并后仍空（空实例=无可用模型不变量）
	if out := MergeBuiltin(nil); len(out) != 0 {
		t.Fatalf("空入参不得注入内置：%d", len(out))
	}
}

func TestResolveFileMerged(t *testing.T) {
	st := &memStore{data: []byte(`{"providers":[{"id":"deepseek","api_key":"sk-x","enabled":true}]}`)}
	ps := ResolveFileMerged(st)
	if len(ps) != 1 || ps[0].BaseURL == "" || ps[0].Dialect != "deepseek" || len(ps[0].Models) != 3 {
		t.Fatalf("文件层合并应得完整 deepseek 条目：%+v", ps)
	}
	// 空配置 = 空（内置不独立出现）
	if ps := ResolveFileMerged(&memStore{}); len(ps) != 0 {
		t.Fatalf("空配置不得出内置：%d", len(ps))
	}
	// 原样档不受影响
	if ps := ResolveFile(st); len(ps) != 1 || ps[0].BaseURL != "" {
		t.Fatalf("自定义档 ResolveFile 应保持原样（未归一补全）：%+v", ps)
	}
}

func TestEffortThinkingFields(t *testing.T) {
	for e, want := range map[string]string{"off": "none", "low": "low", "high": "high", "max": "high"} {
		f := effortThinkingFields(e)
		if len(f) != 1 {
			t.Fatalf("%s 档通用方言应仅 reasoning_effort 一个字段：%v", e, f)
		}
		if f["reasoning_effort"] != want {
			t.Fatalf("%s 档应映射 %s（对齐 OpenAI 词表：off→none 官方关思考值、max→high 宁降档不发错），实得 %v", e, want, f["reasoning_effort"])
		}
	}
}

func TestValidateDialectEffort(t *testing.T) {
	ps := []ProviderSpec{{ID: "p", Kind: "openai", Dialect: "effort", Enabled: true,
		Models: []ModelSpec{{ID: "m", Input: []string{"text"}}}}}
	if err := ValidateProviders(ps); err != nil {
		t.Fatalf("effort 方言应过校验：%v", err)
	}
	ps[0].Dialect = "glm"
	if err := ValidateProviders(ps); err != nil {
		t.Fatalf("glm 方言应过校验：%v", err)
	}
	ps[0].Dialect = "bogus"
	if err := ValidateProviders(ps); err == nil || !strings.Contains(err.Error(), "dialect") {
		t.Fatalf("未知方言应拒：%v", err)
	}
	// 极简条目（仅 ID+密钥，其余靠合并档内置补全）应过校验——PUT 归一副作用
	// 已移除，空 kind 不再被固化成 anthropa 挡掉 by-ID 填充
	minimal := []ProviderSpec{{ID: "deepseek", APIKey: "sk-x", Enabled: true}}
	if err := ValidateProviders(minimal); err != nil {
		t.Fatalf("极简条目应过校验（空位留合并档补全）：%v", err)
	}
	if minimal[0].Kind != "" {
		t.Fatalf("校验不得有归一副作用（空 kind 被固化会挡合并填充）")
	}
}

// extraPreset 应用层扩展预置桩（业务私有厂家典型形态：kind/base_url/
// 模型清单全套自带，密钥归用户层）。
func extraPreset() []ProviderSpec {
	return []ProviderSpec{{
		ID: "extra", Name: "扩展厂家", Kind: "openai", BaseURL: "https://gw.internal/v1",
		Enabled: true,
		Models: []ModelSpec{
			{ID: "m-a", Input: []string{"text", "image"}},
			{ID: "m-b", Input: []string{"text"}},
		},
	}}
}

func TestMergeProvidersExtra(t *testing.T) {
	// 极简引用（key-only）：扩展目录同内置 by-ID 补全
	st := &memStore{data: []byte(`{"providers":[{"id":"extra","api_key":"k","enabled":true}]}`)}
	ps := ResolveFileMergedWith(st, extraPreset())
	if len(ps) != 1 || ps[0].BaseURL != "https://gw.internal/v1" || ps[0].Kind != "openai" || len(ps[0].Models) != 2 {
		t.Fatalf("扩展预置应 by-ID 补全：%+v", ps)
	}
	if ps[0].APIKey != "k" || !ps[0].Enabled {
		t.Fatalf("密钥/启用权不得受预置影响：%+v", ps[0])
	}
	// 空配置仍空：扩展预置与内置同样不独立注入（不变量保住）
	if ps := ResolveFileMergedWith(&memStore{}, extraPreset()); len(ps) != 0 {
		t.Fatalf("空配置不得出扩展预置：%d", len(ps))
	}
	// 用户显式值优先：自填 base_url/自裁模型清单保留
	st2 := &memStore{data: []byte(`{"providers":[{"id":"extra","api_key":"k","enabled":true,
		"base_url":"https://proxy.internal/v1","models":[{"id":"m-a","input":["text"]}]}]}`)}
	ps2 := ResolveFileMergedWith(st2, extraPreset())
	if ps2[0].BaseURL != "https://proxy.internal/v1" || len(ps2[0].Models) != 1 || ps2[0].Models[0].ID != "m-a" {
		t.Fatalf("用户显式值应优先（覆写走代理/自裁清单）：%+v", ps2[0])
	}
	// 与内置共存：都引用档合并两目录，互不干扰；未匹配条目原样
	st3 := &memStore{data: []byte(`{"providers":[
		{"id":"deepseek","api_key":"sk-x","enabled":true},
		{"id":"extra","api_key":"k","enabled":true},
		{"id":"vllm","kind":"openai","base_url":"http://10.0.0.2:8000/v1","api_key":"k2","enabled":true,
		 "models":[{"id":"acme-7b","input":["text"]}]}]}`)}
	ps3 := ResolveFileMergedWith(st3, extraPreset())
	if len(ps3) != 3 || ps3[0].Dialect != "deepseek" || ps3[1].BaseURL != "https://gw.internal/v1" ||
		ps3[2].BaseURL != "http://10.0.0.2:8000/v1" {
		t.Fatalf("内置+扩展应同构合并、未匹配原样：%+v", ps3)
	}
	// 旧入口（无 extra）不注入扩展目录——语义不变
	if ps4 := ResolveFileMerged(st3); len(ps4) != 3 || ps4[1].BaseURL != "" {
		t.Fatalf("无扩展入参的旧入口应保持原语义：%+v", ps4)
	}
}

func TestResolveFileCatalog(t *testing.T) {
	// 应用自备完整目录（挑选权）：运营侧存量配置里 deepseek 与 zhipu 两家
	// 都有极简条目，应用目录只挑了 deepseek + 自备扩展厂家——
	st := &memStore{data: []byte(`{"providers":[
		{"id":"deepseek","api_key":"sk-x","enabled":true},
		{"id":"zhipu","api_key":"zp-x","enabled":true},
		{"id":"extra","api_key":"k","enabled":true}]}`)}
	catalog := append(extraPreset(), ProviderSpec{ // 挑选的基座子集 + 私有全集
		ID: "deepseek", Name: "DeepSeek", Kind: "openai",
		BaseURL: "https://api.deepseek.com", Dialect: "deepseek", Enabled: true,
		Models: []ModelSpec{{ID: "deepseek-v4-flash", Input: []string{"text"}}},
	})
	ps := ResolveFileCatalog(st, catalog)
	if len(ps) != 3 {
		t.Fatalf("不得增删条目：%d", len(ps))
	}
	byID := map[string]ProviderSpec{}
	for _, p := range ps {
		byID[p.ID] = p
	}
	if byID["deepseek"].BaseURL != "https://api.deepseek.com" || byID["deepseek"].Dialect != "deepseek" {
		t.Fatalf("目录内的基座供应商应照常补全：%+v", byID["deepseek"])
	}
	if byID["extra"].BaseURL != "https://gw.internal/v1" {
		t.Fatalf("应用私有供应商应照常补全：%+v", byID["extra"])
	}
	// 排除语义：zhipu 不在应用目录——存量条目保持原样（不补全、不可用），
	// 内置有它也不越权生效（Kind=anthropic 是 NormalizeProviders 的中性
	// 默认填充，非目录补全——补全的实质字段是 BaseURL/方言/模型清单）
	if byID["zhipu"].BaseURL != "" || byID["zhipu"].Dialect != "" || len(byID["zhipu"].Models) != 0 {
		t.Fatalf("目录外的基座供应商不得被补全（排除语义）：%+v", byID["zhipu"])
	}
}

func TestRewriteSpec(t *testing.T) {
	var gotP ProviderSpec
	var gotM ModelSpec
	var gotE string
	f := RewriteSpec(func(p ProviderSpec, m ModelSpec, effort string) ModelSpec {
		gotP, gotM, gotE = p, m, effort
		m.ID = m.ID + "-rewritten"
		return m
	})
	p := ProviderSpec{ID: "x", Kind: "openai", BaseURL: "https://x.internal/v1", APIKey: "k"}
	m := ModelSpec{ID: "m-a", Input: []string{"text"}}
	if _, err := f(context.Background(), p, m, "high"); err != nil {
		t.Fatalf("规格改写工厂应可构造（openai 客户端构造不拨网）：%v", err)
	}
	if gotP.ID != "x" || gotM.ID != "m-a" || gotE != "high" {
		t.Fatalf("rewrite 应收齐原始参数：p=%+v m=%+v effort=%s", gotP, gotM, gotE)
	}
	// 入参规格不被改写（读侧纪律）
	if m.ID != "m-a" {
		t.Fatalf("rewrite 不得改写入参规格")
	}
}
