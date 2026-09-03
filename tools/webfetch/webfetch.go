// Package webfetch 提供 web_fetch 工具：URL → 正文 markdown 提取（httprequest
// 只回裸 HTML，真实网页数百 KB 直接爆上下文——本工具做「提取」而非「全文」）。
// 行为参照 deepseek-harness packages/web/tool-web（MIT，fetch+策略）与
// Claude Code WebFetch；URL 策略（域限制/大小上限/超时）按其 web-fetch-http
// policy 思路内联实现。HTML→markdown 走 x/net/html 结构化提取。
package webfetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/jumeng/einox/contract"
	"github.com/jumeng/einox/tools"
	"github.com/jumeng/einox/tools/egress"
)

// Config 配置（零值可用——全部字段有缺省）。
type Config struct {
	Client    *http.Client      // 缺省自建：15s 超时、5 跳重定向、按 Insecure/RootCAs 定 TLS 姿态
	MaxBytes  int64             // 响应体读取上限（缺省 2MB）
	MaxOutput int               // markdown 输出上限 runes（缺省 30000，对齐 read_document）
	UserAgent string            // 缺省 einox/0.1
	Insecure  bool              // 跳过 TLS 证书校验（缺省 false=严格校验——行业缺省；内网自签场景显式开）
	RootCAs   *x509.CertPool    // 追加信任根（企业自签 CA 正解：装根而非跳校验；nil=系统池）
	Egress    *egress.Validator // 出口校验器（nil = 不治理——默认零行为变化，真源 §9；注入后：请求前置校验恒生效，受管 transport〔校验与拨号同源〕+ 重定向重校验**仅在 Client 为缺省自建时接管**——自定义 Client 时 transport/redirect 归注入方自担，前置校验之后的绕行面〔如重定向入私网〕不在治理内）
}

type fetchIn struct {
	URL string `json:"url"`
}

// NewTools 构造 web_fetch（读面：直过审批）。
func NewTools(cfg Config) ([]contract.Tool, error) {
	t, err := tools.InferTool("web_fetch",
		"抓取网页并提取正文为 markdown（标题/段落/列表/代码块/表格/链接保留，来源 URL 标注；超长截断）。适合阅读文档页、wiki、公告等网页内容。仅支持 http/https；需要登录或动态渲染的页面可能拿不到内容。输出 JSON：{ok, title?, markdown, truncated?, error?}。",
		func(ctx context.Context, in fetchIn) (map[string]any, error) {
			return run(ctx, cfg, in)
		})
	if err != nil {
		return nil, err
	}
	return []contract.Tool{tools.WithBehavior(t, contract.BehaviorRead)}, nil
}

func run(ctx context.Context, cfg Config, in fetchIn) (map[string]any, error) {
	u := strings.TrimSpace(in.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fail("仅支持 http/https URL：" + u)
	}
	// 出口前置校验（真源 §9：webfetch 在服务进程内执行，不受 run_command
	// 沙箱约束——注入校验器时这是唯一治理层；受管 transport 在拨号层复核
	// 防 rebinding，重定向目标由 RedirectChecker 复核）
	if cfg.Egress != nil {
		if err := cfg.Egress.CheckURL(u); err != nil {
			return fail(egress.BoundaryNote + "\n" + err.Error())
		}
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	maxOut := cfg.MaxOutput
	if maxOut <= 0 {
		maxOut = 30000
	}
	client := cfg.Client
	if client == nil {
		// TLS 姿态（2026-09-03 定调：缺省严格校验——curl/requests 等行业缺省；
		// 内网自签两条路：RootCAs 装企业根（正解）或 Insecure 显式跳过〔逃生〕）
		tlsConf := &tls.Config{InsecureSkipVerify: cfg.Insecure, RootCAs: cfg.RootCAs}
		client = &http.Client{Timeout: 15 * time.Second}
		if cfg.Egress != nil {
			client.Transport = cfg.Egress.Transport(tlsConf)
			client.CheckRedirect = cfg.Egress.RedirectChecker()
		} else {
			client.Transport = &http.Transport{TLSClientConfig: tlsConf}
			client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("重定向超 5 跳")
				}
				return nil
			}
		}
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "einox/0.1 (web-fetch)"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail("URL 无法解析：" + err.Error())
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return fail("抓取失败：" + truncateRunes(err.Error(), 160))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("HTTP %d（%s）", resp.StatusCode, u))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return fail("读取失败：" + truncateRunes(err.Error(), 160))
	}
	ct := resp.Header.Get("Content-Type")
	out := map[string]any{"url": u, "content_type": ct}
	if !strings.Contains(ct, "html") {
		// 非 HTML（json/xml/text/csv…）：纯文本直读（wikipedia 类 API 返回也走此路）
		text := truncateRunes(string(body), maxOut)
		out["ok"] = true
		out["markdown"] = text
		out["truncated"] = len([]rune(string(body))) > maxOut
		return out, nil
	}
	title, full := extractMarkdown(body)
	md := truncateRunes(full, maxOut)
	out["ok"] = true
	if title != "" {
		out["title"] = title
	}
	out["markdown"] = md
	out["truncated"] = len([]rune(full)) > maxOut
	return out, nil
}

func fail(msg string) (map[string]any, error) {
	return map[string]any{"ok": false, "error": msg}, nil // 回喂模型自纠
}

// extractMarkdown HTML → 正文 markdown：去脚本样式与页面框架件（nav/footer/
// aside/header），优先 main/article 容器；块级元素映射 markdown，行内保留
// 链接/加粗/斜角/代码。
func extractMarkdown(body []byte) (title, md string) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", ""
	}
	var walkTitle func(*html.Node)
	walkTitle = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "title" && n.FirstChild != nil {
			title = strings.TrimSpace(n.FirstChild.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walkTitle(c)
		}
	}
	walkTitle(doc)

	root := pickMain(doc)
	var b strings.Builder
	render(&b, root, 0)
	out := strings.TrimSpace(b.String())
	// 压连续空行
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return title, out
}

// dropTags 页面框架/噪声件。
var dropTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"nav": true, "footer": true, "aside": true, "header": true,
	"form": true, "button": true, "input": true, "select": true, "svg": true, "iframe": true,
}

// pickMain 选正文容器：main > article > body（去框架件的根）。
func pickMain(doc *html.Node) *html.Node {
	var find func(*html.Node, string) *html.Node
	find = func(n *html.Node, tag string) *html.Node {
		if n.Type == html.ElementNode && n.Data == tag {
			return n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if r := find(c, tag); r != nil {
				return r
			}
		}
		return nil
	}
	for _, tag := range []string{"main", "article"} {
		if n := find(doc, tag); n != nil {
			return n
		}
	}
	return find(doc, "body")
}

var headings = map[string]int{"h1": 1, "h2": 2, "h3": 3, "h4": 4, "h5": 5, "h6": 6}

// render 块级遍历渲染。indent = 列表嵌套层。
func render(b *strings.Builder, n *html.Node, indent int) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.CommentNode {
			continue
		}
		if c.Type != html.ElementNode {
			if strings.TrimSpace(c.Data) != "" {
				b.WriteString(strings.TrimSpace(c.Data))
			}
			continue
		}
		tag := c.Data
		if dropTags[tag] {
			continue
		}
		switch {
		case headings[tag] > 0:
			b.WriteString("\n\n" + strings.Repeat("#", headings[tag]) + " ")
			renderInline(b, c)
			b.WriteString("\n")
		case tag == "p":
			b.WriteString("\n\n")
			renderInline(b, c)
		case tag == "br":
			b.WriteString("\n")
		case tag == "hr":
			b.WriteString("\n\n---\n")
		case tag == "pre":
			b.WriteString("\n\n```\n")
			writeText(b, c)
			b.WriteString("\n```\n")
		case tag == "blockquote":
			b.WriteString("\n\n> ")
			renderInline(b, c)
		case tag == "ul" || tag == "ol":
			renderList(b, c, indent, tag == "ol")
		case tag == "li":
			// 裸 li（父非 ul/ol 已在上面拦截，保险走行内）
			b.WriteString("\n- ")
			renderInline(b, c)
		case tag == "table":
			renderTable(b, c)
		case tag == "dl":
			render(b, c, indent)
		case tag == "dt":
			b.WriteString("\n\n**")
			renderInline(b, c)
			b.WriteString("**")
		case tag == "dd":
			b.WriteString("\n: ")
			renderInline(b, c)
		case tag == "div" || tag == "section" || tag == "main" || tag == "article" ||
			tag == "body" || tag == "span" || tag == "figure" || tag == "figcaption" ||
			tag == "details" || tag == "summary" || tag == "center":
			render(b, c, indent)
		case tag == "img":
			if src, ok := attrOf(c, "src"); ok && src != "" {
				alt, _ := attrOf(c, "alt")
				b.WriteString(fmt.Sprintf("\n\n![%s](%s)\n", alt, src))
			}
		default:
			render(b, c, indent)
		}
	}
}

// renderList 列表（ol 计数保序）。
func renderList(b *strings.Builder, n *html.Node, indent int, ordered bool) {
	i := 0
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.Data != "li" {
			continue
		}
		i++
		pad := strings.Repeat("  ", indent)
		mark := "- "
		if ordered {
			mark = fmt.Sprintf("%d. ", i)
		}
		b.WriteString("\n" + pad + mark)
		render(b, c, indent+1)
	}
	b.WriteString("\n")
}

// renderTable 表格 → 管道行（含表头分隔行）。
func renderTable(b *strings.Builder, tbl *html.Node) {
	var rows [][]string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "tr" {
			var cells []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
					var sb strings.Builder
					renderInline(&sb, c)
					cells = append(cells, strings.Join(strings.Fields(sb.String()), " "))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(tbl)
	if len(rows) == 0 {
		return
	}
	width := 0
	for _, r := range rows {
		if len(r) > width {
			width = len(r)
		}
	}
	b.WriteString("\n\n")
	for i, r := range rows {
		b.WriteString("|")
		for j := 0; j < width; j++ {
			cell := ""
			if j < len(r) {
				cell = r[j]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
		if i == 0 {
			b.WriteString("|" + strings.Repeat("---|", width) + "\n")
		}
	}
}

// renderInline 行内渲染：文本 + a/strong/em/code。
func renderInline(b *strings.Builder, n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			txt := c.Data
			if strings.TrimSpace(txt) == "" {
				if txt != "" {
					b.WriteString(" ") // 纯空白节点 = 行内分隔（</a> 与 后续文本之间）
				}
				continue
			}
			// 折叠内部空白但保留首尾单空格——「段落文本 <a>」的边界空格不可丢
			if isASCIISpace(txt[0]) {
				b.WriteString(" ")
			}
			b.WriteString(strings.Join(strings.Fields(txt), " "))
			if isASCIISpace(txt[len(txt)-1]) {
				b.WriteString(" ")
			}
		case c.Type == html.ElementNode && c.Data == "a":
			href, _ := attrOf(c, "href")
			var sb strings.Builder
			renderInline(&sb, c)
			text := strings.TrimSpace(sb.String())
			if text == "" {
				continue
			}
			if href != "" && !strings.HasPrefix(href, "javascript:") {
				b.WriteString("[" + text + "](" + href + ")")
			} else {
				b.WriteString(text)
			}
		case c.Type == html.ElementNode && (c.Data == "strong" || c.Data == "b"):
			var sb strings.Builder
			renderInline(&sb, c)
			if s := strings.TrimSpace(sb.String()); s != "" {
				b.WriteString("**" + s + "**")
			}
		case c.Type == html.ElementNode && (c.Data == "em" || c.Data == "i"):
			var sb strings.Builder
			renderInline(&sb, c)
			if s := strings.TrimSpace(sb.String()); s != "" {
				b.WriteString("*" + s + "*")
			}
		case c.Type == html.ElementNode && (c.Data == "code" || c.Data == "kbd" || c.Data == "samp"):
			var sb strings.Builder
			renderInline(&sb, c)
			if s := strings.TrimSpace(sb.String()); s != "" {
				b.WriteString("`" + s + "`")
			}
		case c.Type == html.ElementNode && c.Data == "br":
			b.WriteString("\n")
		case c.Type == html.ElementNode && dropTags[c.Data]:
			continue
		case c.Type == html.CommentNode:
			continue
		default:
			renderInline(b, c)
		}
	}
}

// writeText 纯文本收集（pre 块保原文，不折叠空白）。
func writeText(b *strings.Builder, n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			continue
		}
		if c.Type == html.ElementNode && dropTags[c.Data] {
			continue
		}
		writeText(b, c)
	}
}

func attrOf(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…（已截断）"
}
