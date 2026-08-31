package llm

// Package llm 是 LLM provider 领域模型与运行时 resolve 链（自产品
// internal/agent/llmconf.go 迁入；HTTP 面归应用 api 层——本包是组装期唯一
// 真源）。存储经 Store 接口注入（.agent/llm 界面配置层的读写面归应用）。
//
// 加载序（docs/04 定案，每次组装请求前 resolve，改配置下一条请求生效）：
//   LLM_* 环境变量（部署逃生门，构造单一 env provider）
//     > .agent/llm/llm-settings.json（界面配置层，多 Provider）
//     > config/agent.local.yaml（手动层，gitignore；v1 单端点形态）
//     > config/agent.yaml（进仓模板；v1 单端点形态）
//     > 内置缺省（DeepSeek anthropic 端点，flash/pro 两模型）
//
// 模型全局标识 = provider/model 复合键；思考档跟模型走（reasoning.variants）；
// 密钥 write-only（本包只消费不回显，回显脱敏归 api）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jumeng/einox/contract"
)

// Store 界面配置层存储（应用注入——.agent/llm/llm-settings.json 读写面）。
type Store interface {
	ReadLLMFile(name string) ([]byte, bool)
}

// Reasoning 思考档定义（跟模型走；nil = 模型不支持思考）。
type Reasoning struct {
	Variants []string `json:"variants"`
	Default  string   `json:"default"`
}

// Limit 上下文/输出上限（0 = 未知）。
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// ModelSpec 模型条目（id 即模型名；name 空 = 用 id）。
type ModelSpec struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Reasoning *Reasoning `json:"reasoning"`
	Limit     *Limit     `json:"limit,omitempty"`
	Input     []string   `json:"input"` // ⊆ text/image（空 = text）
	Priority  int        `json:"priority"`
	// NoToolCalls 明示该模型不支持函数调用（A4：人工维护元数据，可能过时——
	// fail fast 价值大于标记维护成本）。置位时 assemble 期工具面非空（含会话
	// 域件与 spawn）即 CONFIG 错误，不等首轮运行期报端点方言各异的错；能力
	// 是模型属性故放 ModelSpec 而非 ProviderSpec（同 provider 各模型可不同，
	// Input 能力面同先例）。
	NoToolCalls bool `json:"no_tool_calls,omitempty"`
	// Temperature / TopP 采样参数（nil = 不发字段走端点默认——多数推理端点
	// 拒绝显式 temperature，只在用户显式设置时下发；随会话模型快照粘住，
	// 会话内不变即前缀缓存友好）。建议二选一，不同时设。
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

// ProviderSpec 端点条目。
type ProviderSpec struct {
	ID      string      `json:"id"` // 稳定标识（复合键引用，不可改）
	Name    string      `json:"name"`
	Kind    string      `json:"kind"` // anthropic | openai | responses（responses 形式选项,组装未实现）
	BaseURL string      `json:"base_url"`
	APIKey  string      `json:"api_key"`           // write-only
	Dialect string      `json:"dialect,omitempty"` // 思考方言("" 零发送走端点默认 | "deepseek" 私有块+档位直传 | "glm" 智谱同形私有块 | "effort" 通用 reasoning_effort)——厂家私有格式只走 dialect,绝不写进通用分支
	Enabled bool        `json:"enabled"`
	Catalog []string    `json:"catalog,omitempty"` // 拉取到的端点模型清单（fetch-models 结果缓存,随保存持久化;UI 下拉数据源,不参与校验/路由）
	Models  []ModelSpec `json:"models"`
}

// Settings llm-settings.json 存储结构（公共配置；无默认模型字段——默认值属用户 prefs）。
type Settings struct {
	Providers []ProviderSpec `json:"providers"`
}

// UserPrefs 用户偏好（契约形态——会话模型快照同型）。
type UserPrefs = contract.UserPrefs

// ModelOpt llm-options 平铺模型条目（key = provider/model 复合键；思考是用户级
// 开关,模型条目不携带 reasoning——2026-08-21 模型配置去思考参数定案）。
type ModelOpt struct {
	Key           string   `json:"key"`
	Provider      string   `json:"provider"`
	ProviderName  string   `json:"provider_name"`
	Name          string   `json:"name"`
	Input         []string `json:"input"`
	ContextWindow int      `json:"context_window"` // 上下文窗口（Limit.Context 显式值 > 推断表；0=未知只显 token 绝对数）
}

// builtinModels / builtinDefaultEffort 旧结构迁移用（v1 清单缺省）。
var builtinModels = []string{"deepseek-v4-flash", "deepseek-v4-pro"}

const builtinDefaultEffort = "low"

// SettingsV1 旧单端点结构（2026-08-21 前存量；json/yaml 双标签——yaml 层同形态）。
type SettingsV1 struct {
	API          string    `json:"api" yaml:"api"`
	BaseURL      string    `json:"base_url" yaml:"base_url"`
	APIKey       string    `json:"api_key" yaml:"api_key"`
	DefaultModel string    `json:"default_model" yaml:"default_model"` // 迁移丢弃（默认值改属用户）
	Thinking     string    `json:"thinking" yaml:"thinking"`
	Models       []string  `json:"models" yaml:"models"`
	Vision       *VisionV1 `json:"vision" yaml:"vision"`
}

// VisionV1 旧独立图片端点。
type VisionV1 struct {
	API     string `json:"api" yaml:"api"`
	BaseURL string `json:"base_url" yaml:"base_url"`
	APIKey  string `json:"api_key" yaml:"api_key"`
	Model   string `json:"model" yaml:"model"`
}

// v1Populated v1 结构是否携带可迁移内容。
func v1Populated(old *SettingsV1) bool {
	return old.API != "" || old.BaseURL != "" || old.APIKey != "" || len(old.Models) > 0 || old.Vision != nil
}

// MigrateV1 旧单端点 → providers：main 承接主端点与模型清单（思考档统一
// low/high/max，默认档取旧 thinking——max 原样、其余归 low）；vision 独立
// provider，模型标图片输入。
func MigrateV1(old SettingsV1) []ProviderSpec {
	models := old.Models
	if len(models) == 0 {
		models = builtinModels
	}
	defEffort := builtinDefaultEffort // 旧「关/未选」→ 默认低档
	if old.Thinking == "max" {
		defEffort = "max" // 旧「开」= enabled+effort max
	}
	kind := old.API
	if kind == "" {
		kind = "anthropic"
	}
	main := ProviderSpec{ID: "main", Name: "主端点", Kind: kind, BaseURL: old.BaseURL, APIKey: old.APIKey, Enabled: true}
	for i, id := range models {
		main.Models = append(main.Models, ModelSpec{
			ID:        id,
			Reasoning: &Reasoning{Variants: []string{"low", "high", "max"}, Default: defEffort},
			Input:     []string{"text"}, Priority: 100 + i,
		})
	}
	out := []ProviderSpec{main}
	if old.Vision != nil {
		vKind := old.Vision.API
		if vKind == "" {
			vKind = "openai"
		}
		out = append(out, ProviderSpec{
			ID: "vision", Name: "图片输入端点", Kind: vKind,
			BaseURL: old.Vision.BaseURL, APIKey: old.Vision.APIKey, Enabled: true,
			Models: []ModelSpec{{
				ID:        old.Vision.Model,
				Reasoning: &Reasoning{Variants: []string{"low", "high", "max"}, Default: "low"},
				Input:     []string{"text", "image"}, Priority: 200,
			}},
		})
	}
	return out
}

// ReadStored 原样读 llm-settings.json（无回退；GET 回显与 PUT merge 用）。
// 坏 JSON 容错回空——密钥文件手工损坏时页面可重新保存修复。
func ReadStored(st Store) Settings {
	var s Settings
	if data, ok := st.ReadLLMFile("llm-settings.json"); ok {
		_ = json.Unmarshal(data, &s)
	}
	return s
}

// NormalizeProviders 归一（kind/input 空时填默认），不校验（迁移产物兜底用）。
func NormalizeProviders(ps []ProviderSpec) []ProviderSpec {
	for i := range ps {
		if ps[i].Kind == "" {
			ps[i].Kind = "anthropic"
		}
		for j := range ps[i].Models {
			if len(ps[i].Models[j].Input) == 0 {
				ps[i].Models[j].Input = []string{"text"}
			}
		}
	}
	return ps
}

// ValidateProviders PUT 校验（kind 值域/id 唯一/variants 与 default/input 值域）。
// 纯校验零归一副作用——归一是读侧唯一职责（ResolveFile/ResolveFileMerged 的
// NormalizeProviders）；PUT 侧归一落盘会把空 kind 固化成 anthropic，挡掉
// 合并档「空位补全」的 by-ID 参数填充（2026-08-28 引用策略定案配套）。
func ValidateProviders(ps []ProviderSpec) error {
	pids := map[string]bool{}
	for i := range ps {
		p := ps[i]
		if p.ID == "" {
			return errors.New("provider id 不能为空")
		}
		if pids[p.ID] {
			return fmt.Errorf("provider id 重复：%s", p.ID)
		}
		pids[p.ID] = true
		if p.Kind != "" && p.Kind != "anthropic" && p.Kind != "openai" && p.Kind != "responses" {
			return fmt.Errorf("%s kind 取值应为 anthropic / openai / responses", p.ID)
		}
		if p.Dialect != "" && p.Dialect != "deepseek" && p.Dialect != "glm" && p.Dialect != "effort" {
			return fmt.Errorf("%s dialect 取值应为空、deepseek、glm 或 effort（通用思考方言）", p.ID)
		}
		mids := map[string]bool{}
		for j := range p.Models {
			m := p.Models[j]
			if m.ID == "" {
				return fmt.Errorf("%s 模型 id 不能为空", p.ID)
			}
			if mids[m.ID] {
				return fmt.Errorf("%s 模型 id 重复：%s", p.ID, m.ID)
			}
			mids[m.ID] = true
			for _, in := range m.Input {
				if in != "" && in != "text" && in != "image" {
					return fmt.Errorf("%s/%s input 取值应为 text / image", p.ID, m.ID)
				}
			}
			if m.Reasoning != nil {
				if len(m.Reasoning.Variants) == 0 {
					return fmt.Errorf("%s/%s reasoning.variants 不能为空", p.ID, m.ID)
				}
				if !contains(m.Reasoning.Variants, m.Reasoning.Default) {
					return fmt.Errorf("%s/%s reasoning.default 应在 variants 内", p.ID, m.ID)
				}
			}
		}
	}
	return nil
}

// ResolveFile 界面配置层 resolve（api/llm-options、chat 会话快照、prefs 校验
// 共用）：文件存储的 providers 原样（含 v1 识别迁移）；空配置 = 空。引用策略
// 的都引用档走 ResolveFileMerged（2026-08-28 定案，见 presets.go）。
func ResolveFile(st Store) []ProviderSpec {
	return NormalizeProviders(resolveFileRaw(st))
}

// resolveFileRaw 文件层原始读（含 v1 识别迁移；不归一——Kind 空位留给合并档
// 的内置补全，先归一会误填 anthropic）。
func resolveFileRaw(st Store) []ProviderSpec {
	data, _ := st.ReadLLMFile("llm-settings.json")
	var s Settings
	if data != nil {
		_ = json.Unmarshal(data, &s)
	}
	if len(s.Providers) > 0 {
		return s.Providers
	}
	if data != nil {
		var old SettingsV1
		if json.Unmarshal(data, &old) == nil && v1Populated(&old) {
			return MigrateV1(old)
		}
	}
	return nil
}

// Resolve 运行时完整 resolve（组装期调用）：env > 界面层 > yaml 手动层。
// 全空 = 空（assemble 层给「未配置模型供应商」错误面）。
func Resolve(st Store) []ProviderSpec {
	if p, ok := envProvider(); ok {
		return []ProviderSpec{p}
	}
	if ps := ResolveFile(st); len(ps) > 0 {
		return ps
	}
	return resolveYAML()
}

// envProvider LLM_* 环境变量逃生门（最高优先）：任一字段设置即构造单一
// env provider（kind/模型缺省对齐内置）。
func envProvider() (ProviderSpec, bool) {
	base := os.Getenv("LLM_BASE_URL")
	key := os.Getenv("LLM_API_KEY")
	m := os.Getenv("LLM_MODEL")
	if base == "" && key == "" && m == "" {
		return ProviderSpec{}, false
	}
	kind := os.Getenv("LLM_API")
	if kind == "" {
		kind = "anthropic"
	}
	if m == "" {
		m = "deepseek-v4-flash"
	}
	return ProviderSpec{
		ID: "env", Name: "环境变量端点", Kind: kind, BaseURL: base, APIKey: key, Enabled: true,
		Models: []ModelSpec{{
			ID: m, Reasoning: &Reasoning{Variants: []string{"low", "high", "max"}, Default: "low"},
			Input: []string{"text"}, Priority: 100,
		}},
	}, true
}

// resolveYAML yaml 手动层：config/agent.local.yaml（gitignore）> config/agent.yaml
// （进仓模板）；v1 单端点形态（llm: api/base_url/api_key/models/vision）。
// 坏文件/缺文件容错跳过。
func resolveYAML() []ProviderSpec {
	for _, name := range []string{"agent.local.yaml", "agent.yaml"} {
		data, err := os.ReadFile(filepath.Join("config", name))
		if err != nil {
			continue
		}
		var y struct {
			LLM SettingsV1 `yaml:"llm"`
		}
		if yaml.Unmarshal(data, &y) != nil || !v1Populated(&y.LLM) {
			continue
		}
		return MigrateV1(y.LLM)
	}
	return nil
}

// FlattenModels 启用 provider 的模型平铺（priority 序，llm-options 数据源；
// 空配置返回空切片而非 nil——JSON 出 [] 不出 null）。
func FlattenModels(ps []ProviderSpec) []ModelOpt {
	out := make([]ModelOpt, 0)
	for _, p := range ps {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			cw := inferContextWindow(m.ID)
			if m.Limit != nil && m.Limit.Context > 0 {
				cw = m.Limit.Context
			}
			out = append(out, ModelOpt{
				Key: p.ID + "/" + m.ID, Provider: p.ID, ProviderName: p.Name,
				Name: m.Name, Input: m.Input, ContextWindow: cw,
			})
		}
	}
	return out
}

// inferContextWindow 模型上下文窗口推断（best-effort 前缀表；显式
// Limit.Context > 0 优先于本表）。0 = 未知（前端只显 token 绝对数）。
func inferContextWindow(id string) int {
	id = strings.ToLower(id)
	hit := func(xs ...string) bool {
		for _, x := range xs {
			if strings.Contains(id, x) {
				return true
			}
		}
		return false
	}
	switch {
	case hit("claude"):
		return 200_000
	case hit("gemini"):
		return 1_000_000
	case hit("deepseek"): // 官方定价页：现行三模型上下文均 1M（BuiltinProviders 同源）
		return 1_000_000
	case hit("gpt-4.1"):
		return 1_000_000
	case hit("gpt-4o", "gpt-4-turbo"):
		return 128_000
	case hit("o3", "o4"):
		return 200_000
	case hit("qwen"):
		return 131_072
	case hit("glm", "kimi", "moonshot"):
		return 128_000
	}
	return 0
}

// FindOpt 复合键定位；旧裸名（无 /）匹配模型 id 尾段 → 升格。
func FindOpt(models []ModelOpt, key string) *ModelOpt {
	if key == "" {
		return nil
	}
	for i := range models {
		if models[i].Key == key {
			return &models[i]
		}
	}
	if !strings.Contains(key, "/") {
		for i := range models {
			if strings.HasSuffix(models[i].Key, "/"+key) {
				return &models[i]
			}
		}
	}
	return nil
}

// FindSpec 从 provider 集定位复合键的原始条目（模型构造用；带升格语义）。
func FindSpec(ps []ProviderSpec, key string) (ProviderSpec, ModelSpec, bool) {
	for _, p := range ps {
		for _, m := range p.Models {
			if p.ID+"/"+m.ID == key {
				return p, m, true
			}
		}
	}
	if !strings.Contains(key, "/") { // 旧裸名升格
		for _, p := range ps {
			for _, m := range p.Models {
				if m.ID == key {
					return p, m, true
				}
			}
		}
	}
	return ProviderSpec{}, ModelSpec{}, false
}

// ResolveCurrent 用户 prefs 归一：模型无效 → 回退第一模型；effort 是用户级
// 思考档（off | low | high | max，默认 low；2026-08-28 三档化、2026-08-31
// 关档回归四档——旧值 on/max 归一 max、off 恢复关档本义、未知归一默认），
// 切模型不动 effort；mode 归一 manual | plan | auto（默认 plan），三项独立
// 互不联动。
func ResolveCurrent(models []ModelOpt, p UserPrefs) UserPrefs {
	var cur UserPrefs
	if len(models) == 0 {
		cur.Mode = "plan"
		return cur
	}
	cur.Model = models[0].Key
	cur.Effort = "low"
	cur.Mode = "plan"
	if m := FindOpt(models, p.Model); m != nil {
		cur.Model = m.Key
	}
	switch p.Effort {
	case "off":
		cur.Effort = "off"
	case "on", "max":
		cur.Effort = "max"
	case "high":
		cur.Effort = "high"
	case "low":
		cur.Effort = "low"
	}
	switch p.Mode {
	case "manual", "plan", "auto":
		cur.Mode = p.Mode
	}
	return cur
}

// contains 字符串切片包含判定。
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Contains 导出（api 委托复用）。
func Contains(list []string, s string) bool { return contains(list, s) }
