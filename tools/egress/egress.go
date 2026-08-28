// Package egress 提供工具层网络出口治理（Phase S-9，真源
// findings/2026-08-26-einox-sandbox-design.md §9；移植蓝本 = nanobot
// nanobot/security/network.py，344 行 Python 的 Go 直译收敛）。
//
// 缺口：run_command 沙箱的网络控制是内核层全有或全无，内网形态 Network
// 必须开（依赖安装是命令面常态）——此时内核层网络治理为零；且 webfetch
// 等进程级工具在服务进程内执行，天然不受 run_command 沙箱约束——prompt
// injection 可令 agent 抓内网服务（SSRF/内网探测）。本包在应用层补这一层：
//
//   - SSRF 校验器（webfetch 前置）：私网/内部/元数据 CIDR 阻断 + DNS 解析
//     全 IP 校验（非只查首个）+ 校验与拨号同源（rebinding TOCTOU 无窗口的
//     pinning 等价语义）+ redirect 重校验 + IPv6-mapped 归一化 + CIDR 白名单
//     旁路；
//   - 命令串 URL 预检（run_command 前置）：正则提取 URL 逐个过同一校验器，
//     fail-closed；
//   - 模型友好边界文案（拒绝错误写给 LLM 而非人）。
//
// 默认姿态（真源 §9 A3）：机制归基座、可选注入、默认 nil = 不启用——
// nanobot 语境（公网助理，私网是攻击面，默认阻断正确）与内网产品互为
// 镜像：私网段是 PM 的工作面（内网 wiki/包源常态），阻断默认值必须由
// 应用装配层反转（白名单显式授予）。
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// BoundaryNote 模型友好硬边界文案（nanobot WORKSPACE_BOUNDARY_NOTE 三段式
// 措辞标准；真源 §1.4 C5 口径：escalation 梯子落地前指真实出口——ask_user
// 由用户调整配置，禁指「走审批」防指路不存在的路径）。
const BoundaryNote = "网络出口被策略阻断——这是硬边界不是瞬态故障，换工具、换域名或用 shell 技巧重试均无效；如确需访问该资源请用 ask_user 向用户说明理由，由用户调整出口白名单或人工执行。"

// blockedNets 默认阻断段（nanobot _BLOCKED_NETWORKS 同款）：RFC1918 三段 +
// 0/8「本网络」+ 127/8 回环 + 169.254/16 链路本地（云元数据）+ 100.64/10
// CGN + v6 对应段（::/128 可能路由到本机、::1、fc00::/7 ULA、fe80::/10）。
var blockedNets = mustCIDRs([]string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16",
	"::/128", "::1/128", "fc00::/7", "fe80::/10",
})

// Validator 出口校验器（零值 = 纯阻断段模式；白名单经 New 注入）。
type Validator struct {
	allowed []*net.IPNet
}

// New 构造（allowCIDRs = 白名单段，命中即旁路阻断——内网 LLM 端点/包源等
// 工作面例外；坏值报错 fail-closed，交装配层决定拒启）。
func New(allowCIDRs []string) (*Validator, error) {
	v := &Validator{}
	for _, c := range allowCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("白名单 CIDR %q 无效: %w", c, err)
		}
		v.allowed = append(v.allowed, n)
	}
	return v, nil
}

// blocked 单 IP 判定：IPv6-mapped IPv4 归一化（::ffff:127.0.0.1 绕过坑，
// nanobot _normalize_addr 同款）→ 白名单旁路 → 阻断段。
func (v *Validator) blocked(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	for _, n := range v.allowed {
		if n.Contains(ip) {
			return false
		}
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// CheckURL 校验单条 URL：scheme 白名单（http/https）+ 主机名解析全 IP 校验。
// 解析失败即拒（fail-closed——解析不了的域名不因「查无此名」放行）。
func (v *Validator) CheckURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅允许 http/https，得到 %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	return v.CheckHost(host)
}

// CheckHost 主机名校验：字面量 IP 直查；域名解析出的每个 IP 都查（非只查
// 首个——多记录混杂私网地址即拒，nanobot 同款语义）。
func (v *Validator) CheckHost(host string) error {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		if v.blocked(ip) {
			return fmt.Errorf("地址 %s 属私网/内部/元数据阻断段（白名单未含）", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("主机名 %s 解析失败（fail-closed）: %v", host, err)
	}
	for _, ip := range ips {
		if v.blocked(ip) {
			return fmt.Errorf("主机 %s 解析到阻断段地址 %s", host, ip)
		}
	}
	return nil
}

// cmdURLRe 命令串 URL 提取（nanobot _URL_RE 同款：http(s):// 起、空白与
// 引号/shell 管道元字符止——尾部闭合括号等由 url.Parse 容错为路径成分）。
var cmdURLRe = regexp.MustCompile(`(?i)https?://[^\s"'` + "`" + `;|<>]+`)

// CheckCommand 命令串预检（run_command 前置，fail-closed）：提取全部 URL
// 逐个校验；Network 开放形态下这是命令面的唯一网络治理层（真源 §9）。
func (v *Validator) CheckCommand(cmd string) error {
	for _, m := range cmdURLRe.FindAllString(cmd, -1) {
		if err := v.CheckURL(m); err != nil {
			return fmt.Errorf("URL %s: %w", m, err)
		}
	}
	return nil
}

// Transport 受管 transport：DialContext 内解析 → 全 IP 校验 → 拨已校验 IP。
// 校验与拨号消费同一解析结果——校验时公网、连接时重绑内网的 TOCTOU 无窗口
// （nanobot PinnedDNSAsyncTransport 的 pinning 语义在 Go 侧的等价实现）。
func (v *Validator) Transport(tlsCfg *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig: tlsCfg,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("egress 解析失败: %w", err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("egress: %s 无解析结果", host)
			}
			for _, ia := range ips {
				if v.blocked(ia.IP) {
					return nil, fmt.Errorf("egress: %s 拨号被拒（解析到阻断段地址 %s）", host, ia.IP)
				}
			}
			// v4 优先拨（无 happy-eyeballs 竞速——校验过的全集里选第一个
			// 可达形即可，直译语义优先）
			pick := ips[0]
			for _, ia := range ips {
				if ia.IP.To4() != nil {
					pick = ia
					break
				}
			}
			var d net.Dialer
			return d.DialContext(ctx, network, net.JoinHostPort(pick.IP.String(), port))
		},
	}
}

// RedirectChecker 重定向重校验（http.Client CheckRedirect 注入位；跳数上限
// 与 webfetch 既有 5 跳约束同值——注入后由本函数全权接管该约束）。
func (v *Validator) RedirectChecker() func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("重定向超 5 跳")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("重定向目标 scheme 被拒: %q", req.URL.Scheme)
		}
		if err := v.CheckHost(req.URL.Hostname()); err != nil {
			return fmt.Errorf("重定向目标被拒: %w", err)
		}
		return nil
	}
}

func mustCIDRs(specs []string) []*net.IPNet {
	out := make([]*net.IPNet, len(specs))
	for i, s := range specs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("egress: 阻断段常量非法 " + s) // 编译期常量面，测试锚定
		}
		out[i] = n
	}
	return out
}
