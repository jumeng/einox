package webfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jumeng/einox/tools/egress"
)

const pageHTML = `<html><head><title>测试页</title></head><body>
<nav>站点导航 甲乙丙</nav>
<main>
<h1>标题一</h1>
<p>段落文本 <a href="/x">链接文字</a> 与 <strong>加粗文字</strong>。</p>
<ul><li>项甲</li><li>项乙</li></ul>
<pre>fn main() {}</pre>
<table><tr><th>列A</th><th>列B</th></tr><tr><td>1</td><td>2</td></tr></table>
</main>
<footer>页脚噪声</footer>
</body></html>`

func newSrv(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, pageHTML)
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case "/redir":
			http.Redirect(w, r, "/page", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func invoke(t *testing.T, cfg Config, args string) map[string]any {
	t.Helper()
	ts, err := NewTools(cfg)
	if err != nil || len(ts) != 1 {
		t.Fatalf("构造失败：%v", err)
	}
	out, err := ts[0].Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		t.Fatalf("非 JSON 输出：%s", out)
	}
	return m
}

func TestWebFetchHTML(t *testing.T) {
	srv := newSrv(t)
	m := invoke(t, Config{}, fmt.Sprintf(`{"url":%q}`, srv.URL+"/page"))
	if m["ok"] != true {
		t.Fatalf("应成功：%v", m)
	}
	if m["title"] != "测试页" {
		t.Errorf("标题提取错：%v", m["title"])
	}
	md, _ := m["markdown"].(string)
	for _, want := range []string{
		"# 标题一", "段落文本 [链接文字](/x) 与 **加粗文字**", "- 项甲", "- 项乙",
		"```", "fn main() {}", "| 列A | 列B |", "|---|---|",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown 缺 %q：%s", want, md)
		}
	}
	for _, noise := range []string{"站点导航", "页脚噪声"} {
		if strings.Contains(md, noise) {
			t.Errorf("markdown 应剔除框架噪声 %q", noise)
		}
	}
}

func TestWebFetchJSONPassthrough(t *testing.T) {
	srv := newSrv(t)
	m := invoke(t, Config{}, fmt.Sprintf(`{"url":%q}`, srv.URL+"/api"))
	if m["ok"] != true || !strings.Contains(m["markdown"].(string), `"ok":true`) {
		t.Fatalf("JSON 应直读：%v", m)
	}
}

func TestWebFetchErrors(t *testing.T) {
	srv := newSrv(t)
	if m := invoke(t, Config{}, fmt.Sprintf(`{"url":%q}`, srv.URL+"/nope")); m["ok"] != false {
		t.Errorf("404 应失败：%v", m)
	}
	if m := invoke(t, Config{}, `{"url":"file:///etc/passwd"}`); m["ok"] != false {
		t.Errorf("非 http 协议应拒绝：%v", m)
	}
}

func TestWebFetchRedirectAndTruncate(t *testing.T) {
	srv := newSrv(t)
	m := invoke(t, Config{}, fmt.Sprintf(`{"url":%q}`, srv.URL+"/redir"))
	if m["ok"] != true || !strings.Contains(m["markdown"].(string), "标题一") {
		t.Fatalf("重定向应跟随到正文：%v", m)
	}
	m = invoke(t, Config{MaxOutput: 8}, fmt.Sprintf(`{"url":%q}`, srv.URL+"/page"))
	if m["ok"] != true || m["truncated"] != true {
		t.Fatalf("超长应截断并标记：%v", m)
	}
}

// TestEgressGuard 出口治理注入（S-9：注入校验器后私网/回环目标前置拒绝，
// 错误含模型友好边界文案；httptest 监听 127.0.0.1 即天然阻断目标）。
func TestEgressGuard(t *testing.T) {
	v, err := egress.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := NewTools(Config{Egress: v})
	if err != nil {
		t.Fatal(err)
	}
	srv := newSrv(t)
	defer srv.Close()
	var m map[string]any
	out, _ := ts[0].Invoke(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`/page"}`))
	json.Unmarshal([]byte(out), &m)
	if m["ok"] != false {
		t.Fatalf("回环目标应被拒：%v", m)
	}
	if !strings.Contains(m["error"].(string), "硬边界") || !strings.Contains(m["error"].(string), "ask_user") {
		t.Fatalf("错误应含边界文案与真实出口：%v", m["error"])
	}
}
