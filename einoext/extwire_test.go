package einoext

// localOperator.RunCommand 的错误回喂形态测试：非零退出不再上抛裸 Go 错误
// （上游 PyExecutor 会丢弃输出并双重包装成「execute error ×2: exit status N」，
// traceback 全失模型无从自纠），改为退出码+完整输出并入 Stdout 以 nil 错误
// 回喂；成功零输出占位，堵上游「execute result is empty」误判。

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino/components/tool"
)

func newTestOperator(t *testing.T) *localOperator {
	t.Helper()
	return &localOperator{root: t.TempDir()}
}

func TestRunCommandErrFeed(t *testing.T) {
	op := newTestOperator(t)
	ctx := context.Background()

	// 成功直传：输出原样
	co, err := op.RunCommand(ctx, []string{"sh", "-c", "echo hello"})
	if err != nil || co.Stdout != "hello\n" {
		t.Fatalf("成功应原样透传：%v %q", err, co.Stdout)
	}
	// 成功零输出：占位（上游会误报 execute result is empty）
	co, err = op.RunCommand(ctx, []string{"sh", "-c", ""})
	if err != nil || co.Stdout == "" {
		t.Fatalf("成功零输出应占位：%v %q", err, co.Stdout)
	}
	// 非零退出：退出码+完整输出并入 Stdout，nil 错误（回喂自纠）
	co, err = op.RunCommand(ctx, []string{"sh", "-c", "echo boom >&2; exit 3"})
	if err != nil {
		t.Fatalf("非零退出不应上抛错误：%v %q", err, co.Stdout)
	}
	if !strings.Contains(co.Stdout, "[命令失败 退出码 3]") || !strings.Contains(co.Stdout, "boom") {
		t.Fatalf("失败回喂应含退出码与输出：%q", co.Stdout)
	}
}

// TestPyExecutorErrFeed 穿真实上游组件的端到端锚：python_execute 失败时模型
// 拿到的是含 traceback 的文本结果而非裸 exit status。
func TestPyExecutorErrFeed(t *testing.T) {
	if _, lk := exec.LookPath("python3"); lk != nil {
		t.Skip("环境无 python3")
	}
	py, err := commandline.NewPyExecutor(context.Background(), &commandline.PyExecutorConfig{
		Command:  "python3",
		Operator: newTestOperator(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 正常路径不受影响：无失败标记、原样输出
	res, err := py.InvokableRun(ctx, `{"code":"print(1)"}`)
	if err != nil || strings.Contains(res, "命令失败") || !strings.Contains(res, "1") {
		t.Fatalf("正常执行应不受影响：%v %q", err, res)
	}
	// 脚本异常：拿到 traceback 文本（非裸 exit status 错误）
	res, err = py.InvokableRun(ctx, `{"code":"print('x')\nraise SystemExit(7)"}`)
	if err != nil {
		t.Fatalf("脚本失败应以结果文本回喂而非错误：%v %q", err, res)
	}
	if !strings.Contains(res, "[命令失败 退出码 7]") || !strings.Contains(res, "x") {
		t.Fatalf("失败回喂应含退出码与已打印输出：%q", res)
	}
}

// TestBridgedArgErrTranslation 桥接工具（eino-ext 第三方件）的参数错误经
// Bridge 套 ModelArgError 翻译：真实 PyExecutor 的 InvokableRun 会把 unmarshal
// 错误包成「extract argument fail: %w」，errors.As 须穿透该链（python_execute
// 是 2026-08-27 事故主战场）。错误发生在执行前，无需 python3 在场。
func TestBridgedArgErrTranslation(t *testing.T) {
	py, err := commandline.NewPyExecutor(context.Background(), &commandline.PyExecutorConfig{
		Command:  "python3",
		Operator: newTestOperator(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := Bridge([]tool.BaseTool{py})
	if len(ts) != 1 {
		t.Fatalf("应桥接 1 件：%d", len(ts))
	}
	_, err = ts[0].Invoke(context.Background(), json.RawMessage(`{"code":123}`))
	if err == nil || !strings.Contains(err.Error(), "参数类型不符") ||
		!strings.Contains(err.Error(), "code 应为字符串") || !strings.Contains(err.Error(), "实得数字") {
		t.Fatalf("桥接参数错误应翻译为模型向文案：%v", err)
	}
}
