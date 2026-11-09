package upstream

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestCheckAddr(t *testing.T) {
	tests := []struct {
		addr        string
		blocked     bool // allowPrivate=false 时是否拒绝
		blockedOpen bool // allowPrivate=true 时是否仍拒绝
	}{
		// 公网地址：始终放行
		{addr: "8.8.8.8"},
		{addr: "1.1.1.1"},
		{addr: "2001:4860:4860::8888"},
		{addr: "2606:4700::1111"},
		// 默认拒绝的私网/保留地址
		{addr: "127.0.0.1", blocked: true},
		{addr: "10.0.0.1", blocked: true},
		{addr: "172.16.0.1", blocked: true},
		{addr: "192.168.1.1", blocked: true},
		{addr: "169.254.1.1", blocked: true},
		{addr: "100.64.0.1", blocked: true},
		{addr: "0.0.0.0", blocked: true, blockedOpen: true},
		{addr: "192.0.2.1", blocked: true, blockedOpen: true},
		{addr: "198.18.0.1", blocked: true},
		{addr: "198.51.100.1", blocked: true, blockedOpen: true},
		{addr: "203.0.113.1", blocked: true, blockedOpen: true},
		{addr: "224.0.0.1", blocked: true, blockedOpen: true},
		{addr: "240.0.0.1", blocked: true, blockedOpen: true},
		{addr: "255.255.255.255", blocked: true, blockedOpen: true},
		{addr: "::1", blocked: true},
		{addr: "::", blocked: true, blockedOpen: true},
		{addr: "fd00::1", blocked: true},
		{addr: "fe80::1", blocked: true},
		{addr: "ff02::1", blocked: true, blockedOpen: true},
		{addr: "2001:db8::1", blocked: true, blockedOpen: true},
		{addr: "64:ff9b::1", blocked: true, blockedOpen: true},
		{addr: "2002::1", blocked: true, blockedOpen: true},
		{addr: "2001::1", blocked: true, blockedOpen: true},
		{addr: "100::1", blocked: true, blockedOpen: true},
		{addr: "3fff::1", blocked: true, blockedOpen: true},
		{addr: "5f00::1", blocked: true, blockedOpen: true},
		// IPv4-mapped IPv6 必须等同于对应 IPv4
		{addr: "::ffff:127.0.0.1", blocked: true},
		{addr: "::ffff:10.0.0.1", blocked: true},
		{addr: "::ffff:192.168.1.1", blocked: true},
		{addr: "::ffff:8.8.8.8"},
		// 云元数据：即使 AllowPrivate 也拒绝
		{addr: "169.254.169.254", blocked: true, blockedOpen: true},
		{addr: "169.254.170.2", blocked: true, blockedOpen: true},
		{addr: "169.254.0.23", blocked: true, blockedOpen: true},
		{addr: "100.100.100.200", blocked: true, blockedOpen: true},
		{addr: "fd00:ec2::254", blocked: true, blockedOpen: true},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			addr := netip.MustParseAddr(tc.addr)
			err := checkAddr(addr, false, false)
			if tc.blocked && !errors.Is(err, ErrBlocked) {
				t.Fatalf("checkAddr(%s, false) = %v, 期望 ErrBlocked", tc.addr, err)
			}
			if !tc.blocked && err != nil {
				t.Fatalf("checkAddr(%s, false) = %v, 期望放行", tc.addr, err)
			}
			err = checkAddr(addr, true, false)
			if tc.blockedOpen && !errors.Is(err, ErrBlocked) {
				t.Fatalf("checkAddr(%s, true) = %v, 期望仍被 ErrBlocked 拒绝", tc.addr, err)
			}
			if !tc.blockedOpen && err != nil {
				t.Fatalf("checkAddr(%s, true) = %v, 期望放行", tc.addr, err)
			}
		})
	}
}

func TestCheckAddrInvalid(t *testing.T) {
	if err := checkAddr(netip.Addr{}, false, false); !errors.Is(err, ErrBlocked) {
		t.Fatalf("checkAddr(零值, false) = %v, 期望 ErrBlocked", err)
	}
}

func TestCheckAddrBenchmarkNet(t *testing.T) {
	// 默认拒绝基准测试段。
	if err := checkAddr(netip.MustParseAddr("198.18.0.1"), false, false); !errors.Is(err, ErrBlocked) {
		t.Fatalf("checkAddr(198.18.0.1, false, false) = %v, 期望 ErrBlocked", err)
	}
	// 开启 AllowBenchmarkNet 后放行基准测试段。
	if err := checkAddr(netip.MustParseAddr("198.18.0.1"), false, true); err != nil {
		t.Fatalf("checkAddr(198.18.0.1, false, true) = %v, 期望放行", err)
	}
	// 开启后其他私网段仍被拒绝。
	if err := checkAddr(netip.MustParseAddr("10.0.0.1"), false, true); !errors.Is(err, ErrBlocked) {
		t.Fatalf("checkAddr(10.0.0.1, false, true) = %v, 期望 ErrBlocked", err)
	}
	// 云元数据即使在开启时也拒绝。
	if err := checkAddr(netip.MustParseAddr("169.254.169.254"), false, true); !errors.Is(err, ErrBlocked) {
		t.Fatalf("checkAddr(169.254.169.254, false, true) = %v, 期望 ErrBlocked", err)
	}
}

func TestValidateTarget(t *testing.T) {
	tests := []struct {
		raw     string
		wantErr bool
	}{
		{raw: "http://example.com/x"},
		{raw: "https://example.com/x"},
		{raw: "HTTP://EXAMPLE.COM/x"},
		{raw: "ftp://example.com/x", wantErr: true},
		{raw: "file:///etc/passwd", wantErr: true},
		{raw: "ws://example.com/x", wantErr: true},
		{raw: "javascript:alert(1)", wantErr: true},
		{raw: "http:///x", wantErr: true},
		{raw: "http://", wantErr: true},
		{raw: "mailto:a@b.c", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "   ", wantErr: true},
		{raw: "example.com/x", wantErr: true},
		{raw: "//example.com/x", wantErr: true},
		{raw: "not a url", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := parseTarget(tc.raw)
			if tc.wantErr {
				if !errors.Is(err, ErrBlocked) {
					t.Fatalf("parseTarget(%q) = (%v, %v), 期望 ErrBlocked", tc.raw, u, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q) 返回错误: %v", tc.raw, err)
			}
			if u == nil {
				t.Fatalf("parseTarget(%q) 返回 nil URL", tc.raw)
			}
		})
	}
}

func TestParseTargetErrorDoesNotLeakURL(t *testing.T) {
	// 非法 URL 的错误信息不能带原始 URL（可能含敏感 query）。
	_, err := parseTarget("http://exa mple.com/?token=super-secret")
	if err == nil {
		t.Fatal("期望解析失败")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "exa mple") {
		t.Fatalf("错误信息泄露了原始 URL: %v", err)
	}
}

func TestResolveAndValidateLiteralIP(t *testing.T) {
	lookup := newTestLookup(nil)
	ctx := context.Background()

	u, err := parseTarget("http://8.8.8.8/x")
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	addrs, err := resolveAndValidate(ctx, lookup.lookup, u, false, false)
	if err != nil {
		t.Fatalf("resolveAndValidate: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "8.8.8.8" {
		t.Fatalf("addrs = %v, 期望 [8.8.8.8]", addrs)
	}

	// IPv4-mapped IPv6 需要还原成 IPv4 之后再判断，并返回还原后的地址。
	u, err = parseTarget("http://[::ffff:8.8.8.8]/x")
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	addrs, err = resolveAndValidate(ctx, lookup.lookup, u, false, false)
	if err != nil {
		t.Fatalf("resolveAndValidate: %v", err)
	}
	if len(addrs) != 1 || addrs[0].String() != "8.8.8.8" {
		t.Fatalf("addrs = %v, 期望 [8.8.8.8]", addrs)
	}
	if lookup.callCount() != 0 {
		t.Fatalf("字面量 IP 不应触发 DNS 解析, callCount = %d", lookup.callCount())
	}
}

func TestResolveAndValidateHostname(t *testing.T) {
	ctx := context.Background()

	t.Run("全部公网则放行并去重", func(t *testing.T) {
		lookup := newTestLookup(map[string][]netip.Addr{
			"ok.test": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")},
		})
		u, err := parseTarget("http://ok.test/x")
		if err != nil {
			t.Fatalf("parseTarget: %v", err)
		}
		addrs, err := resolveAndValidate(ctx, lookup.lookup, u, false, false)
		if err != nil {
			t.Fatalf("resolveAndValidate: %v", err)
		}
		if len(addrs) != 2 {
			t.Fatalf("addrs = %v, 期望去重后 2 个", addrs)
		}
	})

	t.Run("任一解析结果被拒则整体拒绝", func(t *testing.T) {
		lookup := newTestLookup(map[string][]netip.Addr{
			"mixed.test": {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")},
		})
		u, err := parseTarget("http://mixed.test/x")
		if err != nil {
			t.Fatalf("parseTarget: %v", err)
		}
		if _, err := resolveAndValidate(ctx, lookup.lookup, u, false, false); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, 期望 ErrBlocked", err)
		}
	})

	t.Run("解析失败返回错误", func(t *testing.T) {
		lookup := newTestLookup(nil)
		lookup.setError(errors.New("dns 挂了"))
		u, err := parseTarget("http://nope.test/x")
		if err != nil {
			t.Fatalf("parseTarget: %v", err)
		}
		if _, err := resolveAndValidate(ctx, lookup.lookup, u, false, false); err == nil {
			t.Fatal("期望解析失败")
		}
	})

	t.Run("IPv4-mapped 私网被拒", func(t *testing.T) {
		lookup := newTestLookup(map[string][]netip.Addr{
			"mapped.test": {netip.MustParseAddr("::ffff:192.168.1.1")},
		})
		u, err := parseTarget("http://mapped.test/x")
		if err != nil {
			t.Fatalf("parseTarget: %v", err)
		}
		if _, err := resolveAndValidate(ctx, lookup.lookup, u, false, false); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, 期望 ErrBlocked", err)
		}
	})

	t.Run("带 Zone 的链路本地地址被拒", func(t *testing.T) {
		// netip.ParseAddr 支持 IPv6 zone；链路本地地址必须被拒绝。
		u, err := parseTarget("http://[fe80::1%25en0]/x")
		if err != nil {
			t.Fatalf("parseTarget: %v", err)
		}
		if _, err := resolveAndValidate(ctx, newTestLookup(nil).lookup, u, false, false); !errors.Is(err, ErrBlocked) {
			t.Fatalf("err = %v, 期望 ErrBlocked", err)
		}
	})
}
