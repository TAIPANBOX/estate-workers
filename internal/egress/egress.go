// Package egress is the drain service's only way out of its network
// namespace. A bridge inside the namespace turns the HTTP proxy port that
// git, Go and Claude Code use into the proxy's unix socket; the proxy,
// outside the namespace and as a user of its own, opens a tunnel only to an
// allowlisted host on port 443 whose every address is public.
package egress

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Allow is every host the drain service reaches. Widening it is a reviewed
// change, never a setting.
var Allow = []string{"api.anthropic.com", "github.com", "proxy.golang.org", "sum.golang.org"}

// Proxy answers CONNECT requests.
type Proxy struct {
	Allow   []string
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Log     io.Writer
}

// notInternet are ranges that IsGlobalUnicast and IsPrivate let through:
// "this network", shared address space (the tailnet lives there), IETF
// protocol assignments, benchmarking and reserved.
var notInternet = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Public reports whether a is an address on the internet at large: not
// loopback, private, shared, link-local, multicast or reserved, in either
// family or mapped from one to the other.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range notInternet {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Check decides one CONNECT target: an allowlisted name on port 443 whose
// every address is public, so neither a literal address nor a name that
// resolves to the host reaches it. It returns the address to dial.
func (p Proxy) Check(ctx context.Context, authority string) (string, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", err
	}
	if port != "443" {
		return "", fmt.Errorf("port %s is not 443", port)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !slices.Contains(p.Allow, host) {
		return "", fmt.Errorf("host %q is not on the allowlist", host)
	}
	addrs, err := p.Resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return "", fmt.Errorf("resolve %s: %v", host, err)
	}
	for _, a := range addrs {
		if !Public(a) {
			return "", fmt.Errorf("%s resolves to %s, which is not a public address", host, a)
		}
	}
	return net.JoinHostPort(addrs[0].Unmap().String(), port), nil
}

// Serve answers every connection on l until l is closed.
func (p Proxy) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go p.handle(c)
	}
}

func (p Proxy) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprintf(p.Log, "deny %s %s: only CONNECT\n", req.Method, req.Host)
		io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	addr, err := p.Check(ctx, req.Host)
	if err != nil {
		fmt.Fprintf(p.Log, "deny %s %s: %v\n", req.Method, req.Host, err)
		io.WriteString(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	up, err := p.Dial(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(p.Log, "fail %s via %s: %v\n", req.Host, addr, err)
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	fmt.Fprintf(p.Log, "allow %s via %s\n", req.Host, addr)
	_ = c.SetDeadline(time.Time{})
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	join(c, br, up)
}

// join copies both ways until each side has finished, closing the write
// half of the other so a reader sees the end.
func join(a net.Conn, ar io.Reader, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(b, ar); closeWrite(b); done <- struct{}{} }()
	go func() { io.Copy(a, b); closeWrite(a); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}

// StartBridge listens on a loopback port inside the namespace and joins
// each connection to the proxy's unix socket until ctx ends. It returns the
// proxy URL for HTTPS_PROXY.
func StartBridge(ctx context.Context, socket string) (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() { <-ctx.Done(); l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				u, err := net.Dial("unix", socket)
				if err != nil {
					return
				}
				defer u.Close()
				join(c, c, u)
			}()
		}
	}()
	return "http://" + l.Addr().String(), nil
}
