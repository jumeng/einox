// egress 校验器单测（纯逻辑 + 本地解析面，跨平台可跑；拨号面仅测阻断路径
// ——校验先于拨号，无监听器需求）。
package egress

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestBlockedLiterals(t *testing.T) {
	v, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"http://127.0.0.1/x", "http://10.1.2.3/", "http://172.16.0.9/",
		"http://192.168.1.1/", "http://169.254.169.254/meta", "http://100.64.0.1/",
		"http://0.0.0.0/", "http://[::1]/", "http://[fe80::1]/", "http://[fd00::1]/",
		"http://[::ffff:127.0.0.1]/", // v6-mapped 归一化
		"ftp://8.8.8.8/x",            // scheme 白名单
		"http://[fc00::1]/",
	} {
		if err := v.CheckURL(bad); err == nil {
			t.Errorf("应阻断: %s", bad)
		}
	}
	for _, ok := range []string{"http://8.8.8.8/", "https://1.1.1.1/dns-query"} {
		if err := v.CheckURL(ok); err != nil {
			t.Errorf("公网字面量应放行: %s: %v", ok, err)
		}
	}
}

func TestWhitelistBypass(t *testing.T) {
	v, err := New([]string{"10.0.0.0/8", "172.16.0.0/12"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"http://10.2.43.83:7777/", "http://172.16.1.5/wiki"} {
		if err := v.CheckURL(ok); err != nil {
			t.Errorf("白名单段应旁路: %s: %v", ok, err)
		}
	}
	// 白名单外私网段仍拒
	if err := v.CheckURL("http://192.168.1.1/"); err == nil {
		t.Error("白名单未含段应仍阻断")
	}
	if err := v.CheckURL("http://127.0.0.1/"); err == nil {
		t.Error("回环不在白名单应阻断")
	}
}

func TestNewBadCIDR(t *testing.T) {
	if _, err := New([]string{"10.0.0.0/8", "not-a-cidr"}); err == nil {
		t.Fatal("坏 CIDR 应报错（fail-closed 交装配层拒启）")
	}
}

func TestHostnameResolution(t *testing.T) {
	v, _ := New(nil)
	// localhost 本地解析（/etc/hosts，无网络依赖）→ 127.0.0.1 → 阻断
	if err := v.CheckHost("localhost"); err == nil {
		t.Fatal("localhost 解析到回环应阻断")
	}
	// 解析失败 fail-closed（保留字域名走真实 NXDOMAIN 路径）
	if err := v.CheckHost("nonexistent.invalid"); err == nil {
		t.Fatal("解析失败应 fail-closed 拒绝")
	}
}

func TestCheckCommand(t *testing.T) {
	v, _ := New([]string{"10.0.0.0/8"})
	for _, bad := range []string{
		"curl http://127.0.0.1:8080/x",
		"git clone http://169.254.169.254/latest/meta-data",
		"wget https://[::1]/secret && cat /etc/passwd",
	} {
		if err := v.CheckCommand(bad); err == nil {
			t.Errorf("命令应被预检拦截: %s", bad)
		}
	}
	for _, ok := range []string{
		"echo hello",                             // 无 URL
		"curl http://10.2.43.83:7777/api/health", // 白名单内网（PM 工作面常态）
		"curl http://8.8.8.8/",                   // 公网字面量
	} {
		if err := v.CheckCommand(ok); err != nil {
			t.Errorf("命令应放行: %s: %v", ok, err)
		}
	}
}

func TestTransportDialBlocked(t *testing.T) {
	v, _ := New(nil)
	tr := v.Transport(nil)
	conn, err := tr.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		conn.Close()
		t.Fatal("阻断段拨号应在连接前被拒")
	}
	if !strings.Contains(err.Error(), "egress") {
		t.Fatalf("错误应带 egress 标识: %v", err)
	}
}

func TestRedirectChecker(t *testing.T) {
	v, _ := New(nil)
	check := v.RedirectChecker()
	blocked, _ := http.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	if err := check(blocked, nil); err == nil {
		t.Fatal("重定向到回环应被拒")
	}
	ok, _ := http.NewRequest("GET", "http://8.8.8.8/", nil)
	if err := check(ok, nil); err != nil {
		t.Fatalf("公网重定向应放行: %v", err)
	}
}
