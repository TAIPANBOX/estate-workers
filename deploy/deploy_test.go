// Package deploy holds no code; its tests check that the host files agree
// with each other, because a mismatch there passes every unit test and only
// shows on the host.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/TAIPANBOX/estate-workers/internal/template"
)

var loadCredRe = regexp.MustCompile(`(?m)^LoadCredentialEncrypted=(.*)$`)

// Each service loads its credential from the exact file put-credential.sh
// writes, and uninstall.sh removes that same file. systemd's search by id
// alone does not find a file with a .cred suffix, measured on the host
// 2026-10-30 ("Couldn't read inherited credential").
func TestServicesLoadTheFilePutCredentialWrites(t *testing.T) {
	put := read(t, "put-credential.sh")
	if !strings.Contains(put, "OUT=/etc/credstore.encrypted/$NAME.cred") {
		t.Fatal("put-credential.sh no longer writes /etc/credstore.encrypted/$NAME.cred; update this test with it")
	}
	uninstall := read(t, "uninstall.sh")

	want := map[string]string{
		"claudew.service":      "anthropic-workers",
		"claudew-push.service": "github-app-workers",
	}
	for unit, id := range want {
		m := loadCredRe.FindAllStringSubmatch(read(t, filepath.Join("systemd", unit)), -1)
		if len(m) != 1 {
			t.Errorf("%s: %d LoadCredentialEncrypted lines, want 1", unit, len(m))
			continue
		}
		path := "/etc/credstore.encrypted/" + id + ".cred"
		if got := m[0][1]; got != id+":"+path {
			t.Errorf("%s: LoadCredentialEncrypted=%s, want %s:%s", unit, got, id, path)
		}
		if !strings.Contains(put, id) {
			t.Errorf("put-credential.sh does not accept %s", id)
		}
		if !strings.Contains(uninstall, path) {
			t.Errorf("uninstall.sh does not remove %s", path)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// systemd's RestrictSUIDSGID filter answers openat2 with ENOSYS, and
// bubblewrap then cannot open any path, so Claude Code's shell sandbox fails
// inside the drain service (measured on the host 2026-10-30 by switching
// each property off in turn). NoNewPrivileges still stops a set-uid file
// from granting anything. The push service runs no bubblewrap and keeps it.
func TestDrainServiceLetsBubblewrapOpenPaths(t *testing.T) {
	if strings.Contains(read(t, "systemd/claudew.service"), "\nRestrictSUIDSGID=yes") {
		t.Error("claudew.service sets RestrictSUIDSGID=yes, which breaks bubblewrap")
	}
	unit := read(t, "systemd/claudew.service")
	// Each lays mounts over /proc, and the kernel then refuses the fresh
	// /proc bubblewrap mounts (measured on the host 2026-10-30).
	for _, p := range []string{"ProtectKernelTunables=yes", "ProtectKernelLogs=yes", "ProtectHostname=yes"} {
		if strings.Contains(unit, "\n"+p) {
			t.Errorf("claudew.service sets %s, which stops bubblewrap mounting /proc", p)
		}
	}
	// bubblewrap cannot mask the socket under /run/containerd (0711).
	if !strings.Contains(unit, "\nTemporaryFileSystem=/run/containerd:ro") {
		t.Error("claudew.service does not hide /run/containerd")
	}
	if !strings.Contains(read(t, "systemd/claudew.service"), "\nNoNewPrivileges=yes") {
		t.Error("claudew.service lost NoNewPrivileges=yes")
	}
	if !strings.Contains(read(t, "systemd/claudew-push.service"), "\nRestrictSUIDSGID=yes") {
		t.Error("claudew-push.service lost RestrictSUIDSGID=yes")
	}
}

// systemd creates the socket's directory owned by root with DirectoryMode,
// not by SocketUser, so the drain service could not reach the socket at all
// (measured on the host 2026-10-30: "connect: permission denied"). The
// directory must be traversable; the socket's own mode and group decide who
// may connect.
func TestPushSocketIsReachableByTheDrainServiceOnly(t *testing.T) {
	sock := read(t, "systemd/claudew-push.socket")
	for _, want := range []string{"\nDirectoryMode=0755", "\nSocketMode=0660", "\nSocketUser=claudew-gh", "\nSocketGroup=claudew"} {
		if !strings.Contains(sock, want) {
			t.Errorf("claudew-push.socket lacks %q", strings.TrimSpace(want))
		}
	}
	if !strings.Contains(read(t, "systemd/claudew.service"), "\nUser=claudew\n") {
		t.Error("the drain service no longer runs as claudew, the socket's group")
	}
}

// A bundle the push service exports gets the creating user's primary group
// unless the directory is setgid, and the drain service then cannot read it
// (measured on the host 2026-10-30: "could not open ... .bundle").
func TestExportedBundlesAreReadableByTheDrainService(t *testing.T) {
	if !strings.Contains(read(t, "install.sh"), "\ninstall -d -m 2750 -o claudew-gh -g claudew /var/lib/claudew-gh/export\n") {
		t.Error("install.sh does not make the export directory setgid claudew")
	}
}

// Every program a template's gate starts must be on the host, or the gate
// fails after the model has already been paid for (measured on the host
// 2026-10-30: "bwrap: execvp make: No such file or directory"). A gate that
// starts the repository's own script names a path; Go comes from go.dev,
// pinned by GO_VERSION; anything else comes from the installer's apt line.
func TestInstallerProvidesEveryGateProgram(t *testing.T) {
	tpls, err := template.Load()
	if err != nil {
		t.Fatal(err)
	}
	apt := aptPackages(t, read(t, "install.sh"))
	checked := 0
	for name, tpl := range tpls {
		for _, gate := range tpl.Gates {
			prog := gate[0]
			if strings.Contains(prog, "/") || prog == "go" {
				continue
			}
			checked++
			if !apt[prog] {
				t.Errorf("template %s runs gate %q, but install.sh does not install %s", name, strings.Join(gate, " "), prog)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no template gate starts a host program; this test measured nothing")
	}
}

var aptLineRe = regexp.MustCompile(`(?m)^apt-get install -y -q (.*)$`)

func aptPackages(t *testing.T, install string) map[string]bool {
	t.Helper()
	m := aptLineRe.FindStringSubmatch(install)
	if m == nil {
		t.Fatal("install.sh has no apt-get install line; update this test with it")
	}
	pkgs := map[string]bool{}
	for _, f := range strings.Fields(m[1]) {
		pkgs[strings.Trim(strings.SplitN(f, "=", 2)[0], `"`)] = true
	}
	return pkgs
}

// The egress filter is a table of its own: Docker, Incus and Tailscale
// keep theirs, so it never flushes the ruleset, and a reload replaces only
// this table in one transaction.
func TestEgressFilterOwnsOneTableAndNeverFlushes(t *testing.T) {
	nft := read(t, "claudew-egress.nft")
	if strings.Contains(nft, "flush") {
		t.Error("claudew-egress.nft flushes something")
	}
	for _, line := range strings.Split(nft, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && (f[0] == "table" || f[1] == "table") {
			if !strings.Contains(line, "table inet claudew") {
				t.Errorf("touches another table: %q", line)
			}
		}
	}
	if !strings.Contains(nft, "\ntable inet claudew\ndelete table inet claudew\n") {
		t.Error("a reload does not replace the table in one transaction")
	}
	unit := read(t, "systemd/claudew-egress.service")
	for _, want := range []string{"\nExecStart=/usr/sbin/nft -f /etc/claudew/egress.nft\n",
		"\nExecStop=/usr/sbin/nft delete table inet claudew\n", "\nRemainAfterExit=yes\n", "\nWantedBy=multi-user.target\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("claudew-egress.service lacks %q", strings.TrimSpace(want))
		}
	}
}

// Only packets the worker users send are judged, by a positive match. A
// negated match (skuid != workers accept, then drops) lets packets with no
// full socket, such as the SYN-ACK of the host's own sshd, fall through to
// the drops and cut the host off its own network. The drain user lives in
// a network namespace of its own, so on the host it sends nothing at all.
func TestEgressJudgesOnlyTheWorkersByAPositiveMatch(t *testing.T) {
	nft := read(t, "claudew-egress.nft")
	if strings.Contains(nft, "!=") {
		t.Error("claudew-egress.nft negates a match")
	}
	if got := chainRules(t, nft, "output"); !slices.Equal(got, []string{
		"type filter hook output priority filter; policy accept;",
		`meta skuid "claudew" counter drop`,
		`meta skuid { "claudew-gh", "claudew-proxy" } jump egress`,
	}) {
		t.Errorf("output chain is %q", got)
	}
}

// The push and proxy users reach the router only for DNS, not the host's
// loopback or any other private, shared or link-local address, and the
// internet only over HTTPS. The host's own services listen on loopback
// (measured on the host 2026-10-30: 127.0.0.1:9100 answered claudew), and
// a packet to the host's LAN address leaves through lo too. The router is
// the same address the units' IPAddressAllow names.
func TestEgressAllowsOnlyDNSToTheRouterAndHTTPSOut(t *testing.T) {
	nft := read(t, "claudew-egress.nft")
	if got := chainRules(t, nft, "egress"); !slices.Equal(got, []string{
		"ip daddr $dns meta l4proto { tcp, udp } th dport 53 accept",
		"ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 255.255.255.255 } counter drop",
		"ip6 daddr { ::1, fc00::/7, fe80::/10, ff00::/8 } counter drop",
		"meta l4proto tcp th dport 443 accept",
		"counter drop",
	}) {
		t.Errorf("egress chain is %q", got)
	}
	m := regexp.MustCompile(`(?m)^define dns = (\S+)$`).FindStringSubmatch(nft)
	if m == nil {
		t.Fatal("claudew-egress.nft defines no dns")
	}
	for _, u := range []string{"systemd/claudew-push.service", "systemd/claudew-proxy.service"} {
		if !regexp.MustCompile(`(?m)^IPAddressAllow=` + regexp.QuoteMeta(m[1]) + `/32\b`).MatchString(read(t, u)) {
			t.Errorf("%s does not allow the router %s that the egress filter names", u, m[1])
		}
	}
}

// The workers never run without the filter: both services require it and
// start after it, and the installer and uninstaller handle it.
func TestWorkerServicesRequireTheEgressFilter(t *testing.T) {
	for _, u := range []string{"systemd/claudew.service", "systemd/claudew-push.service", "systemd/claudew-proxy.service"} {
		unit := read(t, u)
		for _, want := range []string{"\nRequires=claudew-egress.service\n", "claudew-egress.service"} {
			if !strings.Contains(unit, want) {
				t.Errorf("%s lacks %q", u, strings.TrimSpace(want))
			}
		}
		if !regexp.MustCompile(`(?m)^After=.*\bclaudew-egress\.service\b`).MatchString(unit) {
			t.Errorf("%s does not start after the egress filter", u)
		}
	}
	install := read(t, "install.sh")
	for _, want := range []string{"install -m 0644 -o root -g root \"$HERE/claudew-egress.nft\" /etc/claudew/egress.nft",
		"systemctl enable --now claudew-egress.service"} {
		if !strings.Contains(install, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	if !strings.Contains(read(t, "uninstall.sh"), "systemctl disable --now claudew-egress.service") {
		t.Error("uninstall.sh leaves the egress filter running")
	}
}

// chainRules returns the trimmed, non-comment lines of one chain.
func chainRules(t *testing.T, nft, chain string) []string {
	t.Helper()
	start := strings.Index(nft, "\tchain "+chain+" {\n")
	if start < 0 {
		t.Fatalf("no chain %s", chain)
	}
	body := nft[start+len("\tchain "+chain+" {\n"):]
	body = body[:strings.Index(body, "\n\t}")]
	var rules []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	return rules
}

// The drain service, where the model and the gates run, has a network
// namespace of its own with nothing but loopback: none of the host's
// services on 127.0.0.1 or ::1 is in it. Its one door out is the egress
// proxy's unix socket, named the same in the socket unit and the config.
func TestDrainServiceLivesInItsOwnNetworkNamespace(t *testing.T) {
	unit := read(t, "systemd/claudew.service")
	for _, want := range []string{"\nPrivateNetwork=yes\n", "\nIPAddressDeny=any\n", "\nIPAddressAllow=localhost\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("claudew.service lacks %q", strings.TrimSpace(want))
		}
	}
	if !regexp.MustCompile(`(?m)^Wants=.*\bclaudew-proxy\.socket\b`).MatchString(unit) ||
		!regexp.MustCompile(`(?m)^After=.*\bclaudew-proxy\.socket\b`).MatchString(unit) {
		t.Error("claudew.service does not want and follow the proxy socket")
	}
	sock := regexp.MustCompile(`(?m)^ListenStream=(\S+)$`).FindStringSubmatch(read(t, "systemd/claudew-proxy.socket"))
	if sock == nil {
		t.Fatal("claudew-proxy.socket listens on nothing")
	}
	if !strings.Contains(read(t, "config.example.json"), `"proxy_socket": "`+sock[1]+`"`) {
		t.Errorf("config.example.json does not name the proxy socket %s", sock[1])
	}
}

// The proxy runs as a user of its own, so neither the model's user nor the
// key holder's can change what it lets through, and it reaches no loopback
// or private address itself; only the drain service's group may connect.
func TestProxyIsItsOwnUserAndReachesNoLoopback(t *testing.T) {
	unit := read(t, "systemd/claudew-proxy.service")
	for _, want := range []string{"\nUser=claudew-proxy\n", "\nGroup=claudew-proxy\n", " proxy-serve\n",
		"\nRequires=claudew-proxy.socket\n", "\nNoNewPrivileges=yes\n", "\nProtectSystem=strict\n"} {
		if !strings.Contains(unit, want) {
			t.Errorf("claudew-proxy.service lacks %q", strings.TrimSpace(want))
		}
	}
	deny := regexp.MustCompile(`(?m)^IPAddressDeny=(.*)$`).FindStringSubmatch(unit)
	if deny == nil {
		t.Fatal("claudew-proxy.service denies no address")
	}
	for _, p := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "fc00::/7", "fe80::/10"} {
		if !slices.Contains(strings.Fields(deny[1]), p) {
			t.Errorf("claudew-proxy.service does not deny %s", p)
		}
	}
	sock := read(t, "systemd/claudew-proxy.socket")
	for _, want := range []string{"\nSocketUser=claudew-proxy\n", "\nSocketGroup=claudew\n", "\nSocketMode=0660\n", "\nDirectoryMode=0755\n"} {
		if !strings.Contains(sock, want) {
			t.Errorf("claudew-proxy.socket lacks %q", strings.TrimSpace(want))
		}
	}
	install := read(t, "install.sh")
	for _, want := range []string{"useradd --system --gid claudew-proxy", "systemctl enable --now claudew-proxy.socket"} {
		if !strings.Contains(install, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	uninstall := read(t, "uninstall.sh")
	for _, want := range []string{"claudew-proxy.socket", "userdel claudew-proxy"} {
		if !strings.Contains(uninstall, want) {
			t.Errorf("uninstall.sh lacks %q", want)
		}
	}
}
