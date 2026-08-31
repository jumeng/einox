package llm

// 预置引用策略回归（2026-08-28 定案）：MergeBuiltin by-ID 参数补全语义
// （用户显式值优先/密钥与启用权不受内置影响/休眠不注入/入参不改写）+
// ResolveFileMerged 世界观 + 通用思考方言 effortThinkingFields + 校验值域。

import (
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
			Models: []ModelSpec{{ID: "qwen3", Input: []string{"text"}}},
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
	if v.BaseURL != "http://10.0.0.2:8000/v1" || len(v.Models) != 1 || v.Models[0].ID != "qwen3" {
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
