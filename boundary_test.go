package einox

// 边界守卫，两条依赖纪律：
// ① 外部依赖收敛在 approvedModules 白名单内（标准库与自身除外）——新增依赖
//    是有意决策，需同步更新清单，评审天然可见；
// ② contract/ 零 eino import（契约纯度——应用只见契约面，换地基不动应用）。

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
