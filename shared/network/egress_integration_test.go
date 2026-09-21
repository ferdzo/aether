//go:build integration

// Package network integration tests. These are deliberately excluded from the
// default `go test ./...` run: they need root (CAP_NET_ADMIN), /dev/kvm and a
// Firecracker binary plus a bootable kernel/rootfs. They SKIP with an explicit
// message when those prerequisites are missing. They cannot run in an
// unprivileged CI sandbox, and nothing here should be treated as evidence that
// guest networking works until the bridge-mode egress test actually passes.
//
// Run with:
//
//	sudo AETHER_TEST_KERNEL=/path/vmlinux \
//	     AETHER_TEST_ROOTFS=/path/node-rootfs.ext4 \
//	     AETHER_TEST_FIRECRACKER=/path/firecracker \
//	     go test -tags integration -run TestGuestEgressBridgeMode -v ./network
//
// See scripts/test-guest-egress.sh for a documented wrapper.
package network

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"aether/shared/vm"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (CAP_NET_ADMIN); rerun with sudo")
	}
}

func requireRootKVM(t *testing.T) {
	t.Helper()
	requireRoot(t)
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("requires KVM (/dev/kvm): %v", err)
	}
}

// egressAssets resolves the kernel/rootfs/firecracker used for the guest egress
// test, skipping when they are not configured.
func egressAssets(t *testing.T) (kernel, rootfs, firecrackerBin string) {
	t.Helper()
	kernel = firstEnv("AETHER_TEST_KERNEL", "KERNEL_PATH")
	rootfs = firstEnv("AETHER_TEST_ROOTFS", "RUNTIME_PATH")
	firecrackerBin = firstEnv("AETHER_TEST_FIRECRACKER", "FIRECRACKER_BIN")
	if kernel == "" || rootfs == "" {
		t.Skip("set AETHER_TEST_KERNEL and AETHER_TEST_ROOTFS (and optionally AETHER_TEST_FIRECRACKER) to run this test")
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Skipf("kernel image not available: %v", err)
	}
	if _, err := os.Stat(rootfs); err != nil {
		t.Skipf("runtime rootfs not available: %v", err)
	}
	if firecrackerBin == "" {
		firecrackerBin = "firecracker"
	}
	return kernel, rootfs, firecrackerBin
}

// egressHandlerJS is served to the guest as its function entrypoint. It proves
// both requirements from inside the guest: DNS resolution of github.com and an
// outbound HTTPS request. Results are exposed over HTTP on port 3000.
const egressHandlerJS = `
const dns = require('dns');
const https = require('https');
const http = require('http');

const result = { dns: false, dnsAddress: '', https: false, httpsStatus: 0, error: '' };

function serve() {
  http.createServer((req, res) => {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(result));
  }).listen(3000, '0.0.0.0');
}

dns.lookup('github.com', (err, address) => {
  if (err) {
    result.error += 'dns:' + err.message + ';';
  } else {
    result.dns = true;
    result.dnsAddress = address;
  }

  https.get('https://github.com/', (res) => {
    result.https = res.statusCode >= 200 && res.statusCode < 400;
    result.httpsStatus = res.statusCode;
    res.resume();
    serve();
  }).on('error', (e) => {
    result.error += 'https:' + e.message + ';';
    serve();
  });
});
`

type egressResult struct {
	DNS         bool   `json:"dns"`
	DNSAddress  string `json:"dnsAddress"`
	HTTPS       bool   `json:"https"`
	HTTPSStatus int    `json:"httpsStatus"`
	Error       string `json:"error"`
}

// serveGuestHandler serves the entrypoint the runtime /init fetches from the
// gateway (see scripts/prepare-runtime.sh) so the guest can pull it over the
// same network path under test.
func serveGuestHandler(t *testing.T, addr string) *http.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/handler.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = io.WriteString(w, egressHandlerJS)
	})

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	go func() { _ = srv.Serve(ln) }()
	return srv
}

func waitForGuestEgress(t *testing.T, guestIP string, port int, timeout time.Duration) egressResult {
	t.Helper()
	url := fmt.Sprintf("http://%s:%d/", guestIP, port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var res egressResult
			if err := json.Unmarshal(body, &res); err != nil {
				lastErr = fmt.Errorf("decode guest response %q: %w", string(body), err)
				time.Sleep(300 * time.Millisecond)
				continue
			}
			return res
		}
		lastErr = fmt.Errorf("guest returned status %d: %s", resp.StatusCode, string(body))
		time.Sleep(300 * time.Millisecond)
	}

	t.Fatalf("guest did not serve an egress result within %v: %v", timeout, lastErr)
	return egressResult{}
}

// TestGuestEgressBridgeMode boots a real guest through the same bridge + TAP +
// NAT configuration Aether uses by default and asserts that the guest can
// resolve github.com and complete an HTTPS request.
func TestGuestEgressBridgeMode(t *testing.T) {
	requireRootKVM(t)
	kernel, rootfs, firecrackerBin := egressAssets(t)

	bridgeName := firstEnv("AETHER_TEST_BRIDGE", "BRIDGE_NAME")
	if bridgeName == "" {
		bridgeName = "fc-bridge0"
	}
	bridgeCIDR := firstEnv("AETHER_TEST_BRIDGE_CIDR", "BRIDGE_CIDR")
	if bridgeCIDR == "" {
		bridgeCIDR = "172.16.0.0/24"
	}

	bm := NewBridgeManager(bridgeName, bridgeCIDR)
	if err := bm.EnsureBridge(); err != nil {
		t.Fatalf("EnsureBridge: %v", err)
	}

	extIface, err := GetDefaultInterface()
	if err != nil {
		t.Fatalf("GetDefaultInterface: %v", err)
	}
	if err := bm.SetupNAT(extIface); err != nil {
		t.Fatalf("SetupNAT: %v", err)
	}

	guestIP, err := bm.AllocateVMIP()
	if err != nil {
		t.Fatalf("AllocateVMIP: %v", err)
	}
	defer bm.ReleaseVMIP(guestIP)

	tapName := bm.NextTAPName()
	if _, err := bm.CreateTAPDevice(tapName); err != nil {
		t.Fatalf("CreateTAPDevice: %v", err)
	}
	defer bm.DeleteTAPDevice(tapName)
	if err := bm.AttachTAPToBridge(tapName); err != nil {
		t.Fatalf("AttachTAPToBridge: %v", err)
	}

	srv := serveGuestHandler(t, net.JoinHostPort(bm.GetGatewayIP(), "8080"))
	defer srv.Close()

	manager := vm.NewManager(firecrackerBin)
	instance, err := manager.Launch(vm.Config{
		KernelPath:    kernel,
		RootFSPath:    rootfs,
		SocketPath:    fmt.Sprintf("%s/egress-%d.sock", t.TempDir(), os.Getpid()),
		VCPUCount:     1,
		MemSizeMB:     256,
		TAPDeviceName: tapName,
		VMIP:          guestIP,
		GatewayIP:     bm.GetGatewayIP(),
		BootToken:     "aether-egress-test",
		MMDSData: map[string]interface{}{
			"token":      "aether-egress-test",
			"entrypoint": "handler.js",
			"port":       3000,
			"env":        map[string]string{},
			"dns":        []string{"1.1.1.1", "8.8.8.8"},
		},
	})
	if err != nil {
		t.Fatalf("Launch guest: %v", err)
	}
	defer instance.Stop()

	res := waitForGuestEgress(t, guestIP, 3000, 90*time.Second)
	if !res.DNS {
		t.Fatalf("guest could not resolve github.com (error: %s)", res.Error)
	}
	if !res.HTTPS {
		t.Fatalf("guest could not complete an HTTPS request to github.com (status %d, error: %s)", res.HTTPSStatus, res.Error)
	}
	t.Logf("guest egress OK: github.com -> %s, HTTPS %d", res.DNSAddress, res.HTTPSStatus)
}

// TestNetnsSetupLifecycle verifies the netns addressing contract structurally:
// the guest /30 gateway lives on the in-namespace bridge, the TAP is attached to
// it, the host routes the guest /30 down the transport veth, and forwarding is
// enabled inside the namespace. It needs root but not KVM.
func TestNetnsSetupLifecycle(t *testing.T) {
	requireRoot(t)

	m, err := NewNetnsManager("172.31.0.0/16")
	if err != nil {
		t.Fatalf("NewNetnsManager: %v", err)
	}

	in, err := m.Setup(fmt.Sprintf("itest%d", os.Getpid()))
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	tornDown := false
	defer func() {
		if !tornDown {
			m.Teardown(in)
		}
	}()

	hostLink, err := netlink.LinkByName(in.VethHost)
	if err != nil {
		t.Fatalf("host veth %s missing: %v", in.VethHost, err)
	}

	_, guestNet, err := net.ParseCIDR(in.GuestIP + "/30")
	if err != nil {
		t.Fatalf("parse guest network: %v", err)
	}
	routes, err := netlink.RouteList(hostLink, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("RouteList: %v", err)
	}
	foundGuestRoute := false
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == guestNet.String() && r.LinkIndex == hostLink.Attrs().Index {
			foundGuestRoute = true
		}
	}
	if !foundGuestRoute {
		t.Fatalf("no device route for %s via %s", guestNet, in.VethHost)
	}

	nsHandle, err := netns.GetFromName(in.NSName)
	if err != nil {
		t.Fatalf("open namespace %s: %v", in.NSName, err)
	}
	defer nsHandle.Close()

	nlh, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		t.Fatalf("netlink handle in namespace: %v", err)
	}
	defer nlh.Close()

	if _, err := nlh.LinkByName(in.TapName); err != nil {
		t.Fatalf("TAP %s not present inside namespace: %v", in.TapName, err)
	}

	links, err := nlh.LinkList()
	if err != nil {
		t.Fatalf("LinkList in namespace: %v", err)
	}
	foundGateway := false
	for _, l := range links {
		addrs, err := nlh.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if a.IP.String() == in.HostIP {
				foundGateway = true
			}
		}
	}
	if !foundGateway {
		t.Fatalf("guest gateway %s not assigned inside namespace", in.HostIP)
	}

	if got := readForwardingInNamespace(t, nsHandle); got != "1" {
		t.Fatalf("net.ipv4.ip_forward inside namespace = %q, want 1", got)
	}

	m.Teardown(in)
	tornDown = true
	if _, err := netlink.LinkByName(in.VethHost); err == nil {
		t.Fatalf("host veth %s still present after teardown", in.VethHost)
	}
	if _, err := netns.GetFromName(in.NSName); err == nil {
		t.Fatalf("namespace %s still present after teardown", in.NSName)
	}
}

func readForwardingInNamespace(t *testing.T, ns netns.NsHandle) string {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("read current namespace: %v", err)
	}
	defer orig.Close()
	if err := netns.Set(ns); err != nil {
		t.Fatalf("enter namespace: %v", err)
	}
	defer netns.Set(orig)

	data, err := os.ReadFile(forwardSysctlPath)
	if err != nil {
		t.Fatalf("read %s: %v", forwardSysctlPath, err)
	}
	return strings.TrimSpace(string(data))
}
