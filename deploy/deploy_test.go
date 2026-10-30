// Package deploy holds no code; its tests check that the host files agree
// with each other, because a mismatch there passes every unit test and only
// shows on the host.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
