package vm_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aether/shared/vm"
)

// procSyncBuffer is a concurrency-safe io.Writer: the Firecracker process's
// stdout and stderr may be pumped from separate goroutines.
type procSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *procSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *procSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// procRepoRoot returns the repository root, assuming tests run from shared/vm.
func procRepoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return abs
}

func procEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestProcessModeBoot is the real, end-to-end guest process-mode smoke test. It
// boots a real Firecracker microVM whose /init runs
// `aether-env --process sh -c 'echo hello; sleep 2; exit 42'`, and asserts the
// serial console shows the command's output plus the AETHER_EXIT:42 sentinel,
// then that the VM actually exits (the supervisor reboots and reboot=k makes
// Firecracker terminate).
//
// Opt-in: set AETHER_PROCESS_SMOKE=1. Requires /dev/kvm and a job rootfs built
// by scripts/build-job-rootfs.sh (AETHER_TEST_JOB_ROOTFS).
//
// Deliberately no TAP/VMIP/Gateway/BootToken/MMDS: process mode must be
// selectable without MMDS, which is the whole point of this test.
func TestProcessModeBoot(t *testing.T) {
	if os.Getenv("AETHER_PROCESS_SMOKE") != "1" {
		t.Skip("set AETHER_PROCESS_SMOKE=1 to run the real Firecracker process-mode boot")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable (needed for a real boot): %v", err)
	}

	root := procRepoRoot(t)
	kernel := procEnvOr("AETHER_TEST_KERNEL", filepath.Join(root, ".assets", "vmlinux"))
	fcBin := procEnvOr("AETHER_TEST_FIRECRACKER", filepath.Join(root, ".assets", "bin", "firecracker"))
	rootfs := os.Getenv("AETHER_TEST_JOB_ROOTFS")
	if rootfs == "" {
		t.Skip("set AETHER_TEST_JOB_ROOTFS to the job rootfs built by scripts/build-job-rootfs.sh")
	}

	for _, p := range []struct{ path, what string }{
		{kernel, "kernel"},
		{fcBin, "firecracker binary"},
		{rootfs, "job rootfs"},
	} {
		if _, err := os.Stat(p.path); err != nil {
			t.Skipf("%s not found at %s: %v", p.what, p.path, err)
		}
	}

	sockDir, err := os.MkdirTemp("", "aether-proc-smoke-")
	if err != nil {
		t.Fatalf("create socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	console := &procSyncBuffer{}

	cfg := vm.Config{
		KernelPath: kernel,
		RootFSPath: rootfs,
		SocketPath: filepath.Join(sockDir, "fc.sock"),
		VCPUCount:  1,
		MemSizeMB:  128,
		Stdout:     console,
		Stderr:     console,
	}

	machine, err := vm.NewManager(fcBin).Launch(cfg)
	if err != nil {
		t.Fatalf("launch Firecracker: %v\nconsole:\n%s", err, console.String())
	}
	t.Cleanup(func() {
		if machine.Machine != nil {
			_ = machine.Stop()
		}
	})

	// Wait for the VM to exit on its own. The guest reboots after printing the
	// sentinel and reboot=k makes Firecracker terminate.
	done := make(chan error, 1)
	go func() { done <- machine.Wait() }()

	exited := false
	select {
	case waitErr := <-done:
		exited = true
		// A nil error is the expected "Firecracker exited status=0" case: the
		// supervisor rebooted the guest and reboot=k terminated the VMM.
		t.Logf("VM exited, Wait() = %v", waitErr)
		if waitErr != nil {
			t.Errorf("VM exited with an error: %v", waitErr)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("timed out waiting for the VM to exit; console so far:\n%s", console.String())
	}

	out := console.String()
	t.Logf("captured guest console:\n%s", out)

	if !exited {
		t.Fatal("VM did not exit")
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("console does not contain %q", "hello")
	}
	if !strings.Contains(out, "AETHER_EXIT:42") {
		t.Errorf("console does not contain sentinel %q", "AETHER_EXIT:42")
	}
}
