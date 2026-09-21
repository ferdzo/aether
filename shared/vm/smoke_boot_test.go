package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/shared/builder"
)

// syncBuffer is a concurrency-safe sink for the VM console streams, which are
// written from the Firecracker process copy goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// repoAsset returns an env override first, then a repo-relative .assets path.
func repoAsset(t *testing.T, envKey, name string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return filepath.Join(root, ".assets", name)
}

// codeImage builds a tiny read-only code drive containing handler.js using the
// same builder the gateway uses for uploaded function code.
func codeImage(t *testing.T, handlerJS string) string {
	t.Helper()

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "handler.js",
		Mode: 0o644,
		Size: int64(len(handlerJS)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(handlerJS)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	image, err := builder.BuildFromArchive(archive.Bytes(), "code.tar.gz")
	if err != nil {
		t.Fatalf("build code image: %v", err)
	}

	path := filepath.Join(t.TempDir(), "code.ext4")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatalf("write code image: %v", err)
	}
	return path
}

// TestSmokeBootNoNetwork boots a REAL Firecracker microVM with no network
// interface and asserts, from the captured serial console, that:
//   - the kernel + Firecracker binary boot,
//   - the extra drive is attached in order and mounted by the guest /init,
//   - the guest entrypoint (aether-env -> node handler.js) actually runs.
//
// It is opt-in because it needs /dev/kvm and a firecracker binary, and it is
// deliberately network-free so it can run without CAP_NET_ADMIN. This is the
// strongest check available to an unprivileged user; TAP/NAT/MMDS paths still
// require root.
//
//	AETHER_SMOKE_BOOT=1 go test -run TestSmokeBootNoNetwork -v ./vm
func TestSmokeBootNoNetwork(t *testing.T) {
	if os.Getenv("AETHER_SMOKE_BOOT") != "1" {
		t.Skip("set AETHER_SMOKE_BOOT=1 to boot a real Firecracker VM (needs /dev/kvm + firecracker)")
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("KVM not available: %v", err)
	}

	kernel := repoAsset(t, "AETHER_TEST_KERNEL", "vmlinux")
	rootfs := repoAsset(t, "AETHER_TEST_ROOTFS", "node-rootfs.ext4")
	firecrackerBin := repoAsset(t, "AETHER_TEST_FIRECRACKER", "bin/firecracker")
	for _, f := range []string{kernel, rootfs, firecrackerBin} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("asset not available: %s: %v", f, err)
		}
	}

	handler := `process.stdout.write("AETHER_SMOKE_STDOUT_OK\n");
process.stderr.write("AETHER_SMOKE_STDERR_OK\n");
setTimeout(function () { process.exit(0); }, 300);
`
	codeDrive := codeImage(t, handler)

	var stdout, stderr syncBuffer
	v, err := NewManager(firecrackerBin).Launch(Config{
		KernelPath: kernel,
		RootFSPath: rootfs,
		SocketPath: filepath.Join(t.TempDir(), "smoke.sock"),
		VCPUCount:  1,
		MemSizeMB:  256,
		// No TAPDeviceName / VMIP / GatewayIP on purpose: no CAP_NET_ADMIN and
		// no MMDS. BootToken is empty so aether-env uses the legacy argv path.
		Drives: []DriveSpec{{Path: codeDrive, ReadOnly: true}},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("launch VM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- v.Wait() }()

	select {
	case <-done:
		t.Log("VM exited (expected: the node process exits and the kernel resets)")
	case <-time.After(60 * time.Second):
		_ = v.Stop()
		t.Fatalf("VM did not exit within 60s; console so far:\n%s", tail(stdout.String(), 2000))
	}

	out, errOut := stdout.String(), stderr.String()
	if dir := os.Getenv("AETHER_SMOKE_LOGDIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create log dir: %v", err)
		}
		outPath := filepath.Join(dir, "guest-stdout.log")
		errPath := filepath.Join(dir, "guest-stderr.log")
		if err := os.WriteFile(outPath, []byte(out), 0o644); err != nil {
			t.Fatalf("write %s: %v", outPath, err)
		}
		if err := os.WriteFile(errPath, []byte(errOut), 0o644); err != nil {
			t.Fatalf("write %s: %v", errPath, err)
		}
		t.Logf("full console logs written to %s and %s", outPath, errPath)
	}
	t.Logf("captured stdout (%d bytes):\n%s", len(out), tail(out, 2000))
	t.Logf("captured stderr (%d bytes):\n%s", len(errOut), tail(errOut, 1000))

	if !strings.Contains(out+errOut, "AETHER_SMOKE_STDOUT_OK") {
		t.Fatalf("guest handler did not run: marker missing from console output")
	}
	if !strings.Contains(out+errOut, "AETHER_SMOKE_STDERR_OK") {
		t.Fatalf("guest handler stderr marker missing from console output")
	}
	t.Log("guest booted, read the extra drive, and executed the entrypoint")
}

// tail returns at most the last n bytes of s, for readable failure output.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("...[%d bytes truncated]...%s", len(s)-n, s[len(s)-n:])
}
