# 模型面样板(model)

> 清单域 ①:`providers`(必填)/ `fallback-chain` / `vision`。叠加位:[00-skeleton](00-skeleton.md) 的 `Providers` 与 `Options` 同名字段。

## providers 三形态

**形态一:内置目录直用**(`llm.BuiltinProviders()` 内置 DeepSeek 官方推荐端点与智谱 GLM 端点):

```go
Providers: func() []llm.ProviderSpec {
    return llm.BuiltinProviders()
},
```

**形态二:目录合并解析**(`llm.ResolveMerged(st)`:env > 配置文件层 > 内置合并——st 实现 `llm.Store` 的 `ReadLLMFile(name string) ([]byte, bool)`,骨架的 fileStore 加一个方法即可复用):

```go
// fileStore 增补:llm.Store 接口(读 <dataDir>/llm.yaml 等)
func (s *fileStore) ReadLLMFile(name string) ([]byte, bool) {
    return s.ReadUserTreeFile("_config", "llm/"+name)
}

Providers: func() []llm.ProviderSpec {
    return llm.ResolveMerged(store) // 用户显式值优先,密钥/启用权不受内置影响
},
```

**形态三:内联自定义 spec**(骨架形态;自定义 Kind=anthropic 例——DeepSeek /anthropic 端点不预置,可这样接):

```go
Providers: func() []llm.ProviderSpec {
    return []llm.ProviderSpec{
        {
            ID: "my-anthropic", Name: "自定义 Anthropic 端点",
            Kind: "anthropic", // anthropic | openai | responses
            BaseURL: "https://api.example.com",
            APIKey:  os.Getenv("MY_API_KEY"),
            Enabled: true,
            // 思考走协议原生预算档,零方言——Dialect 留空
            Models: []llm.ModelSpec{{ID: "claude-sonnet-4", Name: "Sonnet"}},
        },
    }
},
```

字段速查:`ProviderSpec{ID, Name, Kind, BaseURL, APIKey, Dialect, Enabled, Catalog, Models}`;`ModelSpec{ID, Name, Input(⊆text/image), Priority, NoToolCalls, Temperature/TopP(nil=不发字段)…}`。

## fallback-chain(主模型降级链)

```go
FallbackModels: []string{"deepseek/deepseek-chat", "glm/glm-5.3"}, // provider/model 复合键清单
```

语义:重试耗尽按序换备模型,每档各享完整重连预算;切换发 `model_change` 事件;**致命类(401/403/402)不降级直接停机**;清单错配(键不在 Providers 内)不阻断运行,降级失效 + `harness_note` 留痕。子代理/拓扑子面不挂链。

## vision(图片引用)

前置:模型 `ModelSpec.Input` 含 `"image"`(能力对账——适配不无中生有)。

```go
// ImageResolver = func(path string) ([]byte, string, error),返回字节 + MIME
ImageResolve: func(path string) ([]byte, string, error) {
    // 按应用文档仓库读取面实现;演示形态直接读本地路径
    return os.ReadFile(path), "image/png", nil
},
```

工具结果图片在**模型调用边界**升级为携图 user part(tool 角色不收图;模型不支持时明确报错)。

## 验证

- `go build ./...` 过。
- 冒烟:形态三改 `NewModel: llmtest.New(llmtest.Turn{Text: "ok"}).Factory()` 跑一轮,验证装配链。
- NoToolCalls 模型 × 工具面非空 → assemble 期 CONFIG 错(清单期预检:选了纯文本模型就别配工具面)。
