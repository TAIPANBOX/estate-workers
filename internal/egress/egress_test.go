package egress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

// fakeNet resolves from a table and dials one local echo server, whatever
// the address, recording what was dialled.
type fakeNet struct {
	mu     sync.Mutex
	names  map[string][]netip.Addr
	dialed []string
	echo   string
}

func (f *fakeNet) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	if a, ok := f.names[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func (f *fakeNet) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, addr)
	f.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, "tcp", f.echo)
}

func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func proxyOn(t *testing.T, f *fakeNet) string {
	t.Helper()
	// A short directory: a unix socket path has a length limit.
	dir, err := os.MkdirTemp("/tmp", "egress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "proxy.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p := Proxy{Allow: Allow, Resolve: f.resolve, Dial: f.dial, Log: io.Discard}
	go p.Serve(l)
	return sock
}

func newNet(t *testing.T) *fakeNet {
	return &fakeNet{echo: echoServer(t), names: map[string][]netip.Addr{
		"api.anthropic.com": {netip.MustParseAddr("160.79.104.10")},
		"github.com":        {netip.MustParseAddr("20.26.156.215")},
		"example.com":       {netip.MustParseAddr("93.184.215.14")},
		// An allowlisted name pointed at the host itself, as a rebinding
		// attack or a poisoned resolver would.
		"proxy.golang.org": {netip.MustParseAddr("142.250.1.1"), netip.MustParseAddr("127.0.0.1")},
		"sum.golang.org":   {netip.MustParseAddr("100.100.100.100")},
	}}
}

// connect sends one request line over the socket and returns the status
// line and the connection.
func connect(t *testing.T, sock, req string) (string, net.Conn) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, req)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("%q: %v", req, err)
	}
	return strings.TrimSpace(line), c
}

// The proxy tunnels to an allowlisted host on 443 and to nothing else: not
// another host, not another port, not an allowlisted name that resolves to
// a private, loopback or shared address, and nothing but CONNECT.
func TestProxyTunnelsOnlyToAllowlistedPublicHostsOn443(t *testing.T) {
	f := newNet(t)
	sock := proxyOn(t, f)
	cases := []struct{ req, want string }{
		{"CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n", "200"},
		{"CONNECT GitHub.com.:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\n", "200"},
		{"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n", "403"},
		{"CONNECT github.com:22 HTTP/1.1\r\nHost: github.com:22\r\n\r\n", "403"},
		{"CONNECT github.com:80 HTTP/1.1\r\nHost: github.com:80\r\n\r\n", "403"},
		{"CONNECT proxy.golang.org:443 HTTP/1.1\r\nHost: proxy.golang.org:443\r\n\r\n", "403"},
		{"CONNECT sum.golang.org:443 HTTP/1.1\r\nHost: sum.golang.org:443\r\n\r\n", "403"},
		{"CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n", "403"},
		{"CONNECT 160.79.104.10:443 HTTP/1.1\r\nHost: 160.79.104.10:443\r\n\r\n", "403"},
		{"GET http://github.com/ HTTP/1.1\r\nHost: github.com\r\n\r\n", "405"},
		{"nonsense\r\n\r\n", "400"},
	}
	for _, c := range cases {
		line, _ := connect(t, sock, c.req)
		if !strings.Contains(line, " "+c.want+" ") {
			t.Errorf("%q: got %q, want %s", strings.SplitN(c.req, "\r\n", 2)[0], line, c.want)
		}
	}
	if len(f.dialed) != 2 || f.dialed[0] != "160.79.104.10:443" || f.dialed[1] != "20.26.156.215:443" {
		t.Errorf("dialled %v, want only the two allowed public addresses on 443", f.dialed)
	}
}

// After the 200 the tunnel carries bytes both ways, including bytes the
// client sent right behind the request.
func TestProxyCarriesTheTunnelBothWays(t *testing.T) {
	sock := proxyOn(t, newNet(t))
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\nearly")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	io.WriteString(c, "-late")
	c.(*net.UnixConn).CloseWrite()
	got, _ := io.ReadAll(br)
	if string(got) != "early-late" {
		t.Errorf("tunnel returned %q", got)
	}
}

// Every address that is not on the internet at large is refused.
func TestOnlyInternetAddressesArePublic(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1",
		"100.100.100.100", "169.254.169.254", "fe80::1", "fd00::1", "0.1.2.3", "224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:192.168.1.10", "::ffff:100.64.0.1", "::ffff:198.18.0.1", "0.0.0.0", "::", "198.18.0.1", "192.0.0.8", "255.255.255.255"} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s counts as public", s)
		}
	}
	for _, s := range []string{"160.79.104.10", "20.26.156.215", "2a01:4b00::1", "8.8.8.8"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s does not count as public", s)
		}
	}
}

// The bridge joins a loopback port inside the namespace to the proxy's
// unix socket, so HTTPS_PROXY reaches the proxy.
func TestBridgeJoinsTheLoopbackPortToTheSocket(t *testing.T) {
	sock := proxyOn(t, newNet(t))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	url, err := StartBridge(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("bridge url %s", url)
	}
	c, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT github.com:443 HTTP/1.1\r\nHost: github.com:443\r\n\r\nping")
	br := bufio.NewReader(c)
	if resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("through the bridge: %v %v", resp, err)
	}
	c.(*net.TCPConn).CloseWrite()
	if got, _ := io.ReadAll(br); !bytes.Equal(got, []byte("ping")) {
		t.Errorf("bridge returned %q", got)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second); err == nil {
		t.Error("the bridge still listens after its context ended")
	}
}
