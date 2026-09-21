package vm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A real firecracker.Machine cannot be constructed without launching a
// Firecracker process (which requires KVM), so these tests exercise the
// lifecycle methods with a nil Machine. They verify the context/return
// contract and, importantly, that Stop/Shutdown/Wait never panic and are
// idempotent.

func TestStopNilMachineIsSafeAndCancelsLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	v := &VM{Ctx: ctx, Cancel: cancel}

	if err := v.Stop(); err == nil {
		t.Fatal("Stop() with nil Machine: got nil error, want error")
	}
	if err := v.Stop(); err == nil {
		t.Fatal("second Stop() with nil Machine: got nil error, want error")
	}

	select {
	case <-ctx.Done():
	default:
		t.Fatal("Stop() did not cancel the VM lifetime context")
	}
}

func TestStopNilCancelDoesNotPanic(t *testing.T) {
	v := &VM{}
	if err := v.Stop(); err == nil {
		t.Fatal("Stop() with nil Cancel/Machine: got nil error, want error")
	}
}

func TestShutdownNilMachineIsSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	v := &VM{Ctx: ctx, Cancel: cancel}

	if err := v.Shutdown(); err == nil {
		t.Fatal("Shutdown() with nil Machine: got nil error, want error")
	}
	if err := v.Shutdown(); err == nil {
		t.Fatal("second Shutdown() with nil Machine: got nil error, want error")
	}
}

// Shutdown must not consult the lifetime context for its request: even with a
// pre-cancelled lifetime context it should reach the (nil) Machine path and
// report that the machine is not started rather than surfacing ctx.Canceled.
func TestShutdownIgnoresCancelledLifetimeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := &VM{Ctx: ctx, Cancel: cancel}
	err := v.Shutdown()
	if err == nil {
		t.Fatal("Shutdown() with nil Machine: got nil error, want error")
	}
	if err == context.Canceled {
		t.Fatalf("Shutdown() surfaced lifetime context cancellation: %v", err)
	}
}

func TestWaitNilMachineIsSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &VM{Ctx: ctx, Cancel: cancel}

	if err := v.Wait(); err == nil {
		t.Fatal("Wait() with nil Machine: got nil error, want error")
	}
}

// writeTempDrive creates a real file so buildDrives' existence check passes.
func writeTempDrive(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drive.ext4")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("write temp drive: %v", err)
	}
	return path
}

func TestBuildDrivesRootOnly(t *testing.T) {
	drives, err := buildDrives(Config{RootFSPath: "/rootfs.ext4"})
	if err != nil {
		t.Fatalf("buildDrives: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("got %d drives, want 1", len(drives))
	}
	root := drives[0]
	if got := *root.DriveID; got != "rootfs" {
		t.Errorf("root DriveID = %q, want %q", got, "rootfs")
	}
	if got := *root.PathOnHost; got != "/rootfs.ext4" {
		t.Errorf("root PathOnHost = %q, want %q", got, "/rootfs.ext4")
	}
	if !*root.IsRootDevice {
		t.Error("root IsRootDevice = false, want true")
	}
	if *root.IsReadOnly {
		t.Error("root IsReadOnly = true, want false by default")
	}
}

func TestBuildDrivesRootReadOnly(t *testing.T) {
	drives, err := buildDrives(Config{RootFSPath: "/rootfs.ext4", RootFSReadOnly: true})
	if err != nil {
		t.Fatalf("buildDrives: %v", err)
	}
	if len(drives) != 1 {
		t.Fatalf("got %d drives, want 1", len(drives))
	}
	if !*drives[0].IsReadOnly {
		t.Error("root IsReadOnly = false, want true")
	}
}

func TestBuildDrivesOrderAndFlags(t *testing.T) {
	first := writeTempDrive(t)
	second := writeTempDrive(t)

	drives, err := buildDrives(Config{
		RootFSPath: "/rootfs.ext4",
		Drives: []DriveSpec{
			{Path: first, ReadOnly: true},
			{Path: second, ReadOnly: false},
		},
	})
	if err != nil {
		t.Fatalf("buildDrives: %v", err)
	}
	if len(drives) != 3 {
		t.Fatalf("got %d drives, want 3", len(drives))
	}

	if !*drives[0].IsRootDevice {
		t.Error("drives[0] IsRootDevice = false, want true")
	}
	if *drives[1].IsRootDevice || *drives[2].IsRootDevice {
		t.Error("non-root drive marked IsRootDevice = true")
	}

	if got := *drives[1].PathOnHost; got != first {
		t.Errorf("drives[1] path = %q, want %q (order not preserved)", got, first)
	}
	if got := *drives[2].PathOnHost; got != second {
		t.Errorf("drives[2] path = %q, want %q (order not preserved)", got, second)
	}
	if !*drives[1].IsReadOnly {
		t.Error("drives[1] IsReadOnly = false, want true")
	}
	if *drives[2].IsReadOnly {
		t.Error("drives[2] IsReadOnly = true, want false")
	}

	ids := map[string]bool{}
	for _, d := range drives {
		if ids[*d.DriveID] {
			t.Errorf("duplicate DriveID %q", *d.DriveID)
		}
		ids[*d.DriveID] = true
	}
}

func TestBuildDrivesMissingPathErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.ext4")
	_, err := buildDrives(Config{
		RootFSPath: "/rootfs.ext4",
		Drives:     []DriveSpec{{Path: missing, ReadOnly: true}},
	})
	if err == nil {
		t.Fatal("buildDrives with missing drive path: got nil error, want error")
	}
}

func TestBuildDrivesEmptyPathErrors(t *testing.T) {
	_, err := buildDrives(Config{
		RootFSPath: "/rootfs.ext4",
		Drives:     []DriveSpec{{Path: "", ReadOnly: true}},
	})
	if err == nil {
		t.Fatal("buildDrives with empty drive path: got nil error, want error")
	}
}
