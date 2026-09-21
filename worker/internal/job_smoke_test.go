package internal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aether/shared/protocol"
	"aether/shared/vm"
)

// jobTestRepoRoot resolves the repository root, assuming tests run from
// worker/internal.
func jobTestRepoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return abs
}

// jobTestAsset resolves an asset from its env override, then the repo .assets.
func jobTestAsset(t *testing.T, envKey, fallback string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return filepath.Join(jobTestRepoRoot(t), ".assets", fallback)
}

// TestJobRunnerRealBoot drives a JobRunner against a REAL, network-free
// Firecracker microVM. The job rootfs (scripts/build-job-rootfs.sh) boots with
// /init running `aether-env --process sh -c 'echo hello; sleep 2; exit 42'`,
// so the console carries "hello" and the sentinel "AETHER_EXIT:42".
//
// It boots with no TAP/VMIP/Gateway/BootToken/MMDS, which works unprivileged,
// and injects the real vm.Wait/vm.Stop as the runner's wait/stop behaviour. The
// jobLog is attached directly as the VM's Stdout/Stderr.
//
//	AETHER_JOB_SMOKE=1 \
//	AETHER_TEST_JOB_ROOTFS=/path/.assets/job-rootfs.ext4 \
//	go test -run TestJobRunnerRealBoot -v -count=1 -timeout 180s ./internal
func TestJobRunnerRealBoot(t *testing.T) {
	if os.Getenv("AETHER_JOB_SMOKE") != "1" {
		t.Skip("set AETHER_JOB_SMOKE=1 to run the real Firecracker job boot")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm unavailable (needed for a real boot): %v", err)
	}

	kernel := jobTestAsset(t, "AETHER_TEST_KERNEL", "vmlinux")
	rootfs := jobTestAsset(t, "AETHER_TEST_JOB_ROOTFS", "job-rootfs.ext4")
	firecrackerBin := jobTestAsset(t, "AETHER_TEST_FIRECRACKER", filepath.Join("bin", "firecracker"))

	for _, a := range []struct{ path, what string }{
		{kernel, "kernel"},
		{rootfs, "job rootfs"},
		{firecrackerBin, "firecracker binary"},
	} {
		if _, err := os.Stat(a.path); err != nil {
			t.Skipf("%s not available at %s: %v", a.what, a.path, err)
		}
	}

	// No nonce: the job rootfs uses the explicit --process path, whose sentinel
	// is "AETHER_EXIT:<code>".
	console := newJobLog(defaultJobLogBytes, "")

	machine, err := vm.NewManager(firecrackerBin).Launch(vm.Config{
		KernelPath: kernel,
		RootFSPath: rootfs,
		SocketPath: filepath.Join(t.TempDir(), "job.sock"),
		VCPUCount:  1,
		MemSizeMB:  128,
		// No TAPDeviceName / VMIP / GatewayIP / BootToken / MMDS on purpose.
		Stdout: console,
		Stderr: console,
	})
	if err != nil {
		t.Fatalf("launch Firecracker: %v\nconsole:\n%s", err, console.Tail())
	}
	t.Cleanup(func() { _ = machine.Stop() })

	var recorded []protocol.JobRecord
	runner := NewJobRunner(JobRunnerConfig{
		JobID:     "job-smoke-1",
		RequestID: "req-smoke-1",
		WorkerID:  "worker-smoke",
		Timeout:   90 * time.Second,
		Log:       console,
		Wait:      machine.Wait,
		Stop:      machine.Stop,
		Record: func(rec protocol.JobRecord) error {
			recorded = append(recorded, rec)
			return nil
		},
	})

	rec := runner.Run(context.Background())
	t.Logf("captured guest console tail:\n%s", console.Tail())

	// Raw evidence for the report.
	for _, line := range strings.Split(console.Tail(), "\n") {
		if strings.Contains(line, "hello") || strings.Contains(line, "AETHER_EXIT") {
			t.Logf("RAW CONSOLE: %s", line)
		}
	}

	if rec.State != protocol.JobStateDone {
		t.Fatalf("state = %q, want done; record=%+v", rec.State, rec)
	}
	if rec.ExitCode != 42 {
		t.Fatalf("exit_code = %d, want 42; record=%+v", rec.ExitCode, rec)
	}
	if !strings.Contains(console.Tail(), "hello") {
		t.Fatalf("console tail does not contain the workload output %q", "hello")
	}
	if len(recorded) != 1 {
		t.Fatalf("record callback called %d times, want 1", len(recorded))
	}
	if recorded[0].State != protocol.JobStateDone || recorded[0].ExitCode != 42 {
		t.Fatalf("recorded = %+v, want done/42", recorded[0])
	}
}
