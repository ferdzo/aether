package vm

import (
	"context"
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
