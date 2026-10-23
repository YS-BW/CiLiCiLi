package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

// lookupFunc 把域名解析为 IP 列表。测试通过替换 Client.lookup 注入假解析器，
// 因此单测不需要访问外网。
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// defaultLookup 使用系统解析器；结果会被 SSRF 校验后固定下来用于建连。
func defaultLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return nil, errors.New("空主机名")
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
