package einox

// 边界守卫，三条依赖纪律：
// ① 外部依赖收敛在 approvedModules 白名单内（标准库与自身除外）——新增依赖
//    是有意决策，需同步更新清单，评审天然可见；
// ② contract/ 零 eino import（契约纯度——应用只见契约面，换地基不动应用）；
// ③ 内部包间依赖方向断言（dsh 生成式 module-graph 门禁的 Go 对位）：分层规则
//    机器强制，新增依赖边与白名单同理——显式决策、评审可见。仅查生产 import
//    （不带 -test）：跨层测试依赖（如引擎测试用 llmtest）不属分层语义。

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// approvedModules 允许依赖的外部模块根（= go.mod 直接依赖 + 自身）。
// 清单外模块的任何 import 都会被 TestNoUnexpectedImports 拒绝——防的是
// 对应用/业务仓的反向依赖，与顺手引入的重型依赖。
var approvedModules = []string{
	"github.com/jumeng/einox", // 自身
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/bmatcuk/doublestar/v4",
	"github.com/cloudwego/eino",
	"github.com/cloudwego/eino-ext/components/model/claude",
	"github.com/cloudwego/eino-ext/components/model/openai",
	"github.com/cloudwego/eino-ext/components/tool/bingsearch",
	"github.com/cloudwego/eino-ext/components/tool/browseruse",
	"github.com/cloudwego/eino-ext/components/tool/commandline",
	"github.com/cloudwego/eino-ext/components/tool/duckduckgo",
	"github.com/cloudwego/eino-ext/components/tool/googlesearch",
	"github.com/cloudwego/eino-ext/components/tool/httprequest",
	"github.com/cloudwego/eino-ext/components/tool/mcp",
	"github.com/cloudwego/eino-ext/components/tool/searxng",
	"github.com/cloudwego/eino-ext/components/tool/sequentialthinking",
	"github.com/cloudwego/eino-ext/components/tool/wikipedia",
	"github.com/eino-contrib/jsonschema",
	"github.com/larksuite/oapi-sdk-go/v3", // channels/feishu 渠道适配（MIT）——应用不 import 该包则不进构建
	"github.com/mark3labs/mcp-go",
	"golang.org/x/net",
	"golang.org/x/sys",
	"gopkg.in/yaml.v3",
}

func TestNoUnexpectedImports(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	// -test 含测试文件的 import 集（测试同样守边界）；输出行 = 包路径 \t 依赖清单。
	cmd := exec.Command("go", "list", "-test", "-f",
		"{{.ImportPath}}\t{{range .Imports}}{{.}} {{end}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败（可能无网络/工具链受限）: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg, imports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, imp := range strings.Fields(imports) {
			if strings.HasPrefix(imp, "[") {
				continue // go list -test 的测试变体引用（[pkg.test]），非真实 import
			}
			if !strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
				continue // 标准库（首段无域名点）
			}
			if approvedImport(imp) {
				continue
			}
			t.Errorf("白名单外 import：%s → %s（新依赖请同步更新 approvedModules）", pkg, imp)
		}
	}
}

// approvedImport imp 是否落在白名单模块根之下。
func approvedImport(imp string) bool {
	for _, m := range approvedModules {
		if imp == m || strings.HasPrefix(imp, m+"/") {
			return true
		}
	}
	return false
}

func TestContractZeroEino(t *testing.T) {
	cmd := exec.Command("go", "list", "-f",
		"{{.ImportPath}}\t{{range .Imports}}{{.}} {{end}}", "./contract")
	cmd.Dir, _ = filepath.Abs(".")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		_, imports, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		for _, imp := range strings.Fields(imports) {
			if imp == "github.com/cloudwego/eino" || strings.HasPrefix(imp, "github.com/cloudwego/eino/") {
				t.Errorf("contract 契约面禁 import eino：%s", imp)
			}
		}
	}
}

// modPath 仓库模块根（内部包匹配前缀）。
const modPath = "github.com/jumeng/einox"

// internalDepRules 内部包间禁止的依赖边，规则表与 docs/04 装配面同源：
// engine 是组装根、唯一可依赖基座全家；官方通用渠道件（channels）长在装配位
// 之上（应用侧），同样可依赖 engine；其余基座内包反向依赖引擎即分层穿透。
// src/exemptSrc 限定规则适用的源子树，allow/ban 限定目标子树的禁止与豁免。
var internalDepRules = []struct {
	src       string   // 规则适用的源子树
	ban       string   // 禁止 import 的目标子树
	allow     string   // 目标豁免子树（ban 命中但 allow 命中则放行）
	exemptSrc []string // 豁免的源子树（规则不适用于它们）
	note      string
}{
	{modPath + "/contract", modPath + "/", modPath + "/contract", nil,
		"契约纯度：contract 不依赖任何基座内包"},
	{modPath, modPath + "/engine", "", []string{modPath + "/engine", modPath + "/channels"},
		"engine 是组装根（channels 官方通用件在装配位之上同享豁免），其余包反向依赖引擎即分层穿透"},
	{modPath, modPath + "/channels", modPath + "/channels", nil,
		"官方通用渠道件在装配位之上，基座内包不反向依赖（channels 子树内部自治除外）"},
}

func TestInternalDependencyDirections(t *testing.T) {
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \",\"}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list 失败（可能无网络/工具链受限）: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		pkg, imports, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		for _, imp := range strings.Split(imports, ",") {
			for _, r := range internalDepRules {
				if !pkgUnder(pkg, r.src) || !pkgUnder(imp, r.ban) {
					continue
				}
				if r.allow != "" && pkgUnder(imp, r.allow) {
					continue
				}
				if pkgUnderAny(pkg, r.exemptSrc) {
					continue
				}
				t.Errorf("依赖方向违规（%s）：%s → %s", r.note, pkg, imp)
			}
		}
	}
}

// pkgUnder pkg 是否为 base 自身或其子包。
func pkgUnder(pkg, base string) bool {
	return pkg == base || strings.HasPrefix(pkg, base+"/")
}

func pkgUnderAny(pkg string, bases []string) bool {
	for _, b := range bases {
		if pkgUnder(pkg, b) {
			return true
		}
	}
	return false
}
