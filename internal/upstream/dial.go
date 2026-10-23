package upstream

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// dialPlan 固定一次 DNS 解析的结果：同一请求的所有连接只用这些 IP，
// 不会在校验之后再次独立解析，因此无法被 DNS rebinding 绕过。
// TLS 的 SNI/证书校验仍由 http.Transport 用 URL 里的原始主机名完成。
type dialPlan struct {
	host  string
	addrs []netip.Addr
}

type dialPlanKey struct{}

func withDialPlan(ctx context.Context, plan *dialPlan) context.Context {
	return context.WithValue(ctx, dialPlanKey{}, plan)
}

func dialPlanFrom(ctx context.Context) *dialPlan {
	plan, _ := ctx.Value(dialPlanKey{}).(*dialPlan)
	return plan
}

// dialContext 优先使用请求上下文里固定的 IP 建连。
// 只有上下文里没有匹配的固定计划时才回退到现场解析：这只会发生在
// Options.SafeRedirects=false（标准库跟随重定向）且跳到了另一个主机的场景。
func (c *Client) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if plan := dialPlanFrom(ctx); plan != nil && strings.EqualFold(plan.host, host) {
		return c.dialAddrs(ctx, network, port, plan.addrs)
	}
	addrs, err := c.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("域名 %q 没有解析到任何地址", host)
	}
	return c.dialAddrs(ctx, network, port, addrs)
}

// dialAddrs 依次尝试固定 IP，任一成功即返回。
func (c *Client) dialAddrs(ctx context.Context, network, port string, addrs []netip.Addr) (net.Conn, error) {
	var lastErr error
	for _, addr := range addrs {
		if !addr.IsValid() {
			continue
		}
		addr = addr.Unmap()
		conn, err := c.dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的连接地址")
	}
	return nil, lastErr
}
