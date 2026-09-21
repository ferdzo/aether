package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubScript is a hermetic test double for the external networking binaries
// (ip / iptables / sysctl). It lets the command-generation and idempotency code
// paths be exercised without root:
//
//   - every invocation is appended to $STUB_LOG as "<name> <args>"
//   - iptables keeps a rule database in $STUB_RULES: "-C" probes it and fails
//     when the rule is absent, "-A" records the rule
//   - ip emulates "\-o link show" (cats $STUB_LINKS) and "link show <dev>"
//     (succeeds only when <dev> is listed in $STUB_LINKS)
const stubScript = `#!/bin/sh
name=$(basename "$0")
printf '%s %s\n' "$name" "$*" >> "$STUB_LOG"

if [ "$name" = "iptables" ]; then
  op=""
  key=""
  for a in "$@"; do
    case "$a" in
      -C|-A) op="$a" ;;
      *) key="$key $a" ;;
    esac
  done
  key="${key# }"
  if [ "$op" = "-C" ]; then
    if [ -f "$STUB_RULES" ] && grep -qxF -- "$key" "$STUB_RULES"; then
      exit 0
    fi
    exit 1
  fi
  if [ "$op" = "-A" ]; then
    printf '%s\n' "$key" >> "$STUB_RULES"
    exit 0
  fi
  exit 0
fi

if [ "$name" = "ip" ]; then
  case "$*" in
    "-o link show")
      [ -f "$STUB_LINKS" ] && cat "$STUB_LINKS"
      exit 0
      ;;
    "link show "*)
      dev="$3"
      if [ -f "$STUB_LINKS" ] && grep -qE "(^| )$dev:" "$STUB_LINKS"; then
        printf '%s: <BROADCAST,MULTICAST,UP> mtu 1500\n' "$dev"
        exit 0
      fi
      exit 1
      ;;
  esac
  exit 0
fi

exit 0
`

type stubEnv struct {
	dir     string
	logPath string
}

// newStubEnv writes the stubs into a temp dir, points PATH at it and returns the
// handle used by the assertions. It uses t.Setenv so the environment is restored
// automatically.
func newStubEnv(t *testing.T) stubEnv {
	t.Helper()
	dir := t.TempDir()

	for _, name := range []string{"ip", "iptables", "sysctl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stubScript), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}

	env := stubEnv{
		dir:     dir,
		logPath: filepath.Join(dir, "calls.log"),
	}
	for _, f := range []string{env.logPath, filepath.Join(dir, "links.txt"), filepath.Join(dir, "rules.txt")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatalf("seed %s: %v", f, err)
		}
	}

	t.Setenv("STUB_LOG", env.logPath)
	t.Setenv("STUB_LINKS", filepath.Join(dir, "links.txt"))
	t.Setenv("STUB_RULES", filepath.Join(dir, "rules.txt"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return env
}

// setLinks replaces the emulated `ip -o link show` output.
func (e stubEnv) setLinks(t *testing.T, lines ...string) {
	t.Helper()
	content := ""
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(e.dir, "links.txt"), []byte(content), 0o644); err != nil {
		t.Fatalf("write links: %v", err)
	}
}

// replaceBinary overrides a stub with a fixed failing program.
func (e stubEnv) replaceBinary(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(body), 0o755); err != nil {
		t.Fatalf("replace %s: %v", name, err)
	}
}

func (e stubEnv) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(e.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func requireCall(t *testing.T, calls []string, want string) {
	t.Helper()
	for _, line := range calls {
		if line == want {
			return
		}
	}
	t.Fatalf("missing call %q in:\n%s", want, strings.Join(calls, "\n"))
}

func countCalls(calls []string, substr string) int {
	n := 0
	for _, line := range calls {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

func TestSetupNATBridgeModeIsIdempotent(t *testing.T) {
	env := newStubEnv(t)
	bm := NewBridgeManager("br0", "172.16.0.0/24")

	if err := bm.SetupNAT("eth0"); err != nil {
		t.Fatalf("first SetupNAT: %v", err)
	}
	first := env.calls(t)

	// Second run must probe every rule with -C and add nothing.
	if err := bm.SetupNAT("eth0"); err != nil {
		t.Fatalf("second SetupNAT: %v", err)
	}
	all := env.calls(t)
	second := all[len(first):]

	if got := countCalls(all, "-A "); got != 3 {
		t.Fatalf("expected exactly 3 -A calls across two runs, got %d:\n%s", got, strings.Join(all, "\n"))
	}
	if got := countCalls(second, "-C "); got != 3 {
		t.Fatalf("expected 3 -C probes on the second run, got %d:\n%s", got, strings.Join(second, "\n"))
	}
	if got := countCalls(second, "-A "); got != 0 {
		t.Fatalf("second run must not append rules again:\n%s", strings.Join(second, "\n"))
	}

	requireCall(t, all, "iptables -t nat -A POSTROUTING -s 172.16.0.0/24 -o eth0 -j MASQUERADE")
	requireCall(t, all, "iptables -A FORWARD -i br0 -o eth0 -j ACCEPT")
	requireCall(t, all, "iptables -A FORWARD -i eth0 -o br0 -m state --state RELATED,ESTABLISHED -j ACCEPT")
	requireCall(t, all, "sysctl -w net.ipv4.ip_forward=1")
}

func TestSetupNATNetnsModeScopesBySubnet(t *testing.T) {
	env := newStubEnv(t)
	bm := NewBridgeManager("", "172.31.0.0/16")

	if err := bm.SetupNAT("eth0"); err != nil {
		t.Fatalf("SetupNAT: %v", err)
	}
	ccalls := env.calls(t)

	requireCall(t, ccalls, "iptables -A FORWARD -s 172.31.0.0/16 -o eth0 -j ACCEPT")
	requireCall(t, ccalls, "iptables -A FORWARD -i eth0 -d 172.31.0.0/16 -m state --state RELATED,ESTABLISHED -j ACCEPT")

	// No malformed empty interface selector may be generated.
	if strings.Contains(strings.Join(ccalls, "\n"), "-i  ") {
		t.Fatalf("empty -i selector generated:\n%s", strings.Join(ccalls, "\n"))
	}
}

func TestSetupNATPropagatesIptablesFailure(t *testing.T) {
	env := newStubEnv(t)
	env.replaceBinary(t, "iptables", "#!/bin/sh\necho 'boom: permission denied' >&2\nexit 1\n")

	bm := NewBridgeManager("br0", "172.16.0.0/24")
	err := bm.SetupNAT("eth0")
	if err == nil {
		t.Fatal("expected SetupNAT to fail when iptables fails")
	}
	if !strings.Contains(err.Error(), "boom: permission denied") {
		t.Fatalf("error must carry the command output, got: %v", err)
	}
}

func TestSetupNATPropagatesSysctlFailure(t *testing.T) {
	env := newStubEnv(t)
	env.replaceBinary(t, "sysctl", "#!/bin/sh\necho 'sysctl unavailable' >&2\nexit 3\n")

	bm := NewBridgeManager("br0", "172.16.0.0/24")
	err := bm.SetupNAT("eth0")
	if err == nil {
		t.Fatal("expected SetupNAT to fail when sysctl fails")
	}
	if !strings.Contains(err.Error(), "sysctl unavailable") {
		t.Fatalf("error must carry the command output, got: %v", err)
	}
}

func TestSetupNATRejectsEmptyExternalInterface(t *testing.T) {
	newStubEnv(t)
	bm := NewBridgeManager("br0", "172.16.0.0/24")
	if err := bm.SetupNAT(""); err == nil {
		t.Fatal("expected an error for an empty external interface")
	}
}

func TestNextTAPNameSkipsExistingLinks(t *testing.T) {
	env := newStubEnv(t)
	env.setLinks(t, "1: lo: <LOOPBACK,UP> mtu 65536", "2: tap0: <BROADCAST,UP> mtu 1500", "3: tap1: <BROADCAST,UP> mtu 1500")

	bm := NewBridgeManager("br0", "172.16.0.0/24")
	if got := bm.NextTAPName(); got != "tap2" {
		t.Fatalf("NextTAPName() = %q, want tap2", got)
	}
}

func TestNextTAPNameIgnoresNamespacedTAPs(t *testing.T) {
	env := newStubEnv(t)
	// tapns0 belongs to netns mode and must not shift bridge-mode indexing.
	env.setLinks(t, "1: lo: <LOOPBACK,UP> mtu 65536", "2: tapns0: <BROADCAST,UP> mtu 1500")

	bm := NewBridgeManager("br0", "172.16.0.0/24")
	if got := bm.NextTAPName(); got != "tap0" {
		t.Fatalf("NextTAPName() = %q, want tap0", got)
	}
}

func TestNextTAPNameReusesReleasedIndex(t *testing.T) {
	newStubEnv(t)
	bm := NewBridgeManager("br0", "172.16.0.0/24")

	if got := bm.NextTAPName(); got != "tap0" {
		t.Fatalf("first NextTAPName() = %q, want tap0", got)
	}
	if err := bm.DeleteTAPDevice("tap0"); err != nil {
		t.Fatalf("DeleteTAPDevice: %v", err)
	}
	if got := bm.NextTAPName(); got != "tap0" {
		t.Fatalf("NextTAPName() after release = %q, want tap0", got)
	}
}

func TestCreateTAPDeviceRefusesToAdoptExisting(t *testing.T) {
	env := newStubEnv(t)
	env.setLinks(t, "1: lo: <LOOPBACK,UP> mtu 65536", "2: tap0: <BROADCAST,UP> mtu 1500")

	bm := NewBridgeManager("br0", "172.16.0.0/24")
	if _, err := bm.CreateTAPDevice("tap0"); err == nil {
		t.Fatal("expected CreateTAPDevice to refuse an existing interface")
	}
}

func TestAllocateVMIPSkipsAddressesOfExistingTAPs(t *testing.T) {
	env := newStubEnv(t)
	env.setLinks(t, "1: lo: <LOOPBACK,UP> mtu 65536", "2: tap0: <BROADCAST,UP> mtu 1500", "3: tap2: <BROADCAST,UP> mtu 1500")

	bm := NewBridgeManager("br0", "172.16.0.0/24")

	// tap0 -> 172.16.0.2, tap2 -> 172.16.0.4; the first free address is .3.
	got, err := bm.AllocateVMIP()
	if err != nil {
		t.Fatalf("AllocateVMIP: %v", err)
	}
	if got != "172.16.0.3" {
		t.Fatalf("AllocateVMIP() = %q, want 172.16.0.3", got)
	}

	next, err := bm.AllocateVMIP()
	if err != nil {
		t.Fatalf("second AllocateVMIP: %v", err)
	}
	if next != "172.16.0.5" {
		t.Fatalf("second AllocateVMIP() = %q, want 172.16.0.5", next)
	}
}

func TestTapIndexRejectsNamespacedNames(t *testing.T) {
	if _, ok := tapIndex("tapns3"); ok {
		t.Fatal("tapns3 must not be treated as a bridge-mode TAP index")
	}
	idx, ok := tapIndex("tap7")
	if !ok || idx != 7 {
		t.Fatalf("tapIndex(tap7) = %d, %v", idx, ok)
	}
}
