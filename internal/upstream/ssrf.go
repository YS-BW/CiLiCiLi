package upstream

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// 本文件实现 SSRF 校验：协议只允许 http/https；字面量 IP 与域名解析结果都要
// 逐个校验，拒绝私网/回环/链路本地/保留/CGNAT 地址；AllowPrivate 只放行内网段
// （直播自建源），云厂商元数据地址永远拒绝。

// alwaysBlocked 是无论 AllowPrivate 如何都拒绝的云元数据地址。
var alwaysBlocked = []netip.Prefix{
	netip.MustParsePrefix("169.254.169.254/32"), // AWS/GCP/Azure/OpenStack 元数据
	netip.MustParsePrefix("169.254.170.2/32"),   // ECS 任务元数据
	netip.MustParsePrefix("169.254.0.23/32"),    // 腾讯云元数据
	netip.MustParsePrefix("100.100.100.200/32"), // 阿里云元数据
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IPv6 元数据
}

// privatePrefixes 是 AllowPrivate 时放行的内网/链路本地段。
var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// blockedPrefixes 是默认拒绝的地址段（已包含 privatePrefixes 中的内网段）。
var blockedPrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),       // 本网络（含 0.0.0.0）
	netip.MustParsePrefix("10.0.0.0/8"),      // 私网
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),     // 回环
	netip.MustParsePrefix("169.254.0.0/16"),  // 链路本地
	netip.MustParsePrefix("172.16.0.0/12"),   // 私网
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF 协议分配
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 中继任播
	netip.MustParsePrefix("192.168.0.0/16"),  // 私网
	netip.MustParsePrefix("198.18.0.0/15"),   // 基准测试
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // 组播
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留 + 广播
	// IPv6
	netip.MustParsePrefix("::/96"),          // 未指定/回环/IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"), // 本地 NAT64
	netip.MustParsePrefix("100::/64"),       // 丢弃
	netip.MustParsePrefix("2001::/32"),      // Teredo
	netip.MustParsePrefix("2001:2::/48"),    // 基准测试
	netip.MustParsePrefix("2001:10::/28"),   // ORCHID
	netip.MustParsePrefix("2001:20::/28"),   // ORCHIDv2
	netip.MustParsePrefix("2001:db8::/32"),  // 文档
	netip.MustParsePrefix("2002::/16"),      // 6to4
	netip.MustParsePrefix("3fff::/20"),      // 文档（新）
	netip.MustParsePrefix("5f00::/16"),      // SRv6 SID
	netip.MustParsePrefix("fc00::/7"),       // ULA
	netip.MustParsePrefix("fe80::/10"),      // 链路本地
	netip.MustParsePrefix("ff00::/8"),       // 组播
}

// checkAddr 判断单个 IP 是否允许访问。IPv4-mapped IPv6 先还原为 IPv4 再判断。
func checkAddr(addr netip.Addr, allowPrivate bool) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: 无效 IP", ErrBlocked)
	}
	// 带 zone 的地址（如 fe80::1%en0）先去掉 zone 再做前缀判断，
	// 否则 Prefix.Contains 会因 zone 而失效，导致链路本地地址绕过。
	addr = addr.WithZone("").Unmap()
	if containsPrefix(alwaysBlocked, addr) {
		return fmt.Errorf("%w: %s 是云元数据地址", ErrBlocked, addr)
	}
	if allowPrivate && containsPrefix(privatePrefixes, addr) {
		return nil
	}
	if containsPrefix(blockedPrefixes, addr) {
		return fmt.Errorf("%w: %s 属于禁止访问的地址段", ErrBlocked, addr)
	}
	return nil
}

func containsPrefix(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// validateTarget 校验 URL 的协议与主机名（不含地址校验）。
func validateTarget(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: 目标 URL 为空", ErrBlocked)
	}
	if u.Opaque != "" {
		return fmt.Errorf("%w: 不支持 opaque URL", ErrBlocked)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: 不支持的协议 %q（仅允许 http/https）", ErrBlocked, u.Scheme)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%w: 目标 URL 缺少主机名", ErrBlocked)
	}
	return nil
}

// parseTarget 解析并校验目标 URL。错误信息不拼接原始 URL，避免敏感 query 进日志/响应。
func parseTarget(rawURL string) (*url.URL, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return nil, fmt.Errorf("%w: 目标 URL 为空", ErrBlocked)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: 目标 URL 无法解析", ErrBlocked)
	}
	if err := validateTarget(u); err != nil {
		return nil, err
	}
	return u, nil
}

// resolveAndValidate 解析目标 URL 的地址并逐个校验，返回可用于固定建连的 IP 列表。
// 只要有一个解析结果被拒绝，整个域名就被拒绝（防止攻击者混入内网地址轮询）。
func resolveAndValidate(ctx context.Context, lookup lookupFunc, u *url.URL, allowPrivate bool) ([]netip.Addr, error) {
	if err := validateTarget(u); err != nil {
		return nil, err
	}
	host := u.Hostname()
	// 字面量 IP 不做 DNS 解析，直接校验。
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := checkAddr(addr, allowPrivate); err != nil {
			return nil, err
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	lookupHost := strings.TrimSuffix(host, ".")
	if lookupHost == "" {
		return nil, fmt.Errorf("%w: 目标 URL 缺少主机名", ErrBlocked)
	}
	addrs, err := lookup(ctx, lookupHost)
	if err != nil {
		return nil, fmt.Errorf("解析域名 %q 失败: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("域名 %q 没有解析到任何地址", host)
	}
	out := make([]netip.Addr, 0, len(addrs))
	seen := make(map[netip.Addr]struct{}, len(addrs))
	for _, addr := range addrs {
		if !addr.IsValid() {
			continue
		}
		if err := checkAddr(addr, allowPrivate); err != nil {
			return nil, fmt.Errorf("域名 %q 的解析结果不可用: %w", host, err)
		}
		addr = addr.Unmap()
		if _, ok := seen[addr]; ok {
			continue
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("域名 %q 没有可用地址", host)
	}
	return out, nil
}
