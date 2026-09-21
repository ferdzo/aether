package internal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aether/shared/protocol"
)

// Offline (NET_MODE=none) instances must skip the whole network block. The
// Instance has nil managers on purpose: if NoNetwork were not honoured this
// would dereference a nil bridgeMgr before it could allocate anything.
func TestProvisionNetworkNoNetworkIsNoOp(t *testing.T) {
	inst := &Instance{ID: "inst-offline", FunctionID: "fn-offline"}

	np, err := inst.provisionNetwork(InstanceConfig{NoNetwork: true})
	if err != nil {
		t.Fatalf("provisionNetwork = %v, want nil", err)
	}
	if np.inet != nil || np.tap != nil {
		t.Fatalf("offline instance allocated network resources: %+v", np)
	}
	if np.tapName != "" || np.vmIP != "" || np.gateway != "" || np.guestMask != "" || np.netNSPath != "" {
		t.Fatalf("offline instance resolved network params: %+v", np)
	}
}

// The vm.Config handed to Firecracker for an offline instance must have no NIC
// and no MMDS: empty network fields are what vm.Manager treats as "no NIC".
func TestBuildVMConfigNoNetworkHasNoNICOrMMDS(t *testing.T) {
	inst := &Instance{ID: "inst-offline", FunctionID: "fn-offline"}
	cfg := InstanceConfig{
		KernelPath:  "/kernel",
		RuntimePath: "/rootfs",
		SocketPath:  "/tmp/offline.sock",
		VCPUCount:   1,
		MemSizeMB:   128,
		NoNetwork:   true,
	}

	np, err := inst.provisionNetwork(cfg)
	if err != nil {
		t.Fatalf("provisionNetwork = %v, want nil", err)
	}
	got := buildVMConfig(cfg, np, nil, nil)

	if got.TAPDeviceName != "" || got.VMIP != "" || got.GatewayIP != "" || got.GuestMask != "" || got.NetNSPath != "" {
		t.Fatalf("offline VM must have no network fields: %+v", got)
	}
	if got.BootToken != "" || got.MMDSData != nil {
		t.Fatalf("offline VM must not get boot token/MMDS: token=%q mmds=%v", got.BootToken, got.MMDSData)
	}
	// Sanity: the refactor must still pass the non-network fields through.
	if got.KernelPath != "/kernel" || got.RootFSPath != "/rootfs" || got.SocketPath != "/tmp/offline.sock" {
		t.Fatalf("non-network fields not passed through: %+v", got)
	}
}

// A process job on an offline worker must not emit a boot token or MMDS: the
// guest has no NIC to fetch MMDS from and aether-env fails closed on a token
// it cannot resolve.
func TestStartJobNoNetworkOmitsBootTokenAndMMDS(t *testing.T) {
	w := newJobWorker(t)
	w.cfg.NoNetwork = true

	var gotCfg InstanceConfig
	withProvisionSeams(t, func(_ context.Context, _ *Instance, cfg InstanceConfig) error {
		gotCfg = cfg
		return nil
	}, nil, nil)
	withJobRunnerSeam(t, func(context.Context, *JobRunner) {})

	job := protocol.Job{
		JobID:      "job-offline",
		RequestID:  "req-offline",
		FunctionID: "fn-offline",
		Mode:       jobModeProcess,
		Command:    []string{"true"},
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := w.handleJob(context.Background(), data); err != nil {
		t.Fatalf("handleJob = %v, want nil", err)
	}

	if !gotCfg.NoNetwork {
		t.Fatal("offline job config did not set NoNetwork")
	}
	if gotCfg.BootToken != "" {
		t.Fatalf("offline job must not set BootToken, got %q", gotCfg.BootToken)
	}
	if gotCfg.MMDSData != nil {
		t.Fatalf("offline job must not set MMDSData, got %v", gotCfg.MMDSData)
	}
	if gotCfg.ConsoleWriter == nil {
		t.Fatal("offline job must keep the job log as its console writer")
	}
}

// The function path threads NoNetwork through (HTTP functions still require a
// networked mode). Registration deliberately fails against the dead registry,
// but the captured config proves the plumbing.
func TestSpawnInstanceContextPassesNoNetwork(t *testing.T) {
	const functionID = "fn-offline"
	w := newProvisionWorker(t, functionID)
	w.cfg.NoNetwork = true

	var gotCfg InstanceConfig
	withProvisionSeams(t,
		func(_ context.Context, _ *Instance, cfg InstanceConfig) error {
			gotCfg = cfg
			return nil
		},
		func(context.Context, *Instance, int, time.Duration) error { return nil },
		func(*Instance, int, int) error { return nil },
	)

	_, _ = w.SpawnInstanceContext(context.Background(), functionID)

	if !gotCfg.NoNetwork {
		t.Fatal("SpawnInstanceContext did not pass NoNetwork through")
	}
}

// Run must skip EnsureBridge entirely in offline mode. The bridge name is one
// EnsureBridge would fail to create, so an error would prove it was called.
func TestRunNoNetworkSkipsBridgeSetup(t *testing.T) {
	w, _ := newStreamWorker(t)
	w.cfg.NoNetwork = true
	w.cfg.BridgeName = "aether-offline-must-not-exist"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error in offline mode: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
