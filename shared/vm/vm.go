package vm

import (
	"context"
	"fmt"
	"os"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

type Config struct {
	KernelPath    string
	RootFSPath    string
	CodeDrivePath string
	SocketPath    string
	VCPUCount     int64
	MemSizeMB     int64
	TAPDeviceName string
	VMIP          string
	GatewayIP     string
}

type VM struct {
	Machine *firecracker.Machine
	Config  Config
	Ctx     context.Context
	Cancel  context.CancelFunc
}

type Manager struct {
	FirecrackerBin string
}

func NewManager(firecrackerBin string) *Manager {
	if firecrackerBin == "" {
		firecrackerBin = "firecracker"
	}
	return &Manager{FirecrackerBin: firecrackerBin}
}

func (m *Manager) Launch(cfg Config) (*VM, error) {
	if _, err := os.Stat(cfg.KernelPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kernel not found: %s", cfg.KernelPath)
	}
	if _, err := os.Stat(cfg.RootFSPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("rootfs not found: %s", cfg.RootFSPath)
	}

	if cfg.VCPUCount == 0 {
		cfg.VCPUCount = 1
	}
	if cfg.MemSizeMB == 0 {
		cfg.MemSizeMB = 128
	}

	ctx, cancel := context.WithCancel(context.Background())

	if cfg.SocketPath != "" {
		os.Remove(cfg.SocketPath)
	}

	// Kernel boot args
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/init"
	if cfg.VMIP != "" && cfg.GatewayIP != "" {
		bootArgs = fmt.Sprintf("%s ip=%s::%s:255.255.255.0::eth0:off", bootArgs, cfg.VMIP, cfg.GatewayIP)
	}

	drives := []models.Drive{
		{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(cfg.RootFSPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		},
	}

	if cfg.CodeDrivePath != "" {
		if _, err := os.Stat(cfg.CodeDrivePath); err == nil {
			fmt.Printf("[VM] Adding code drive: %s\n", cfg.CodeDrivePath)
			drives = append(drives, models.Drive{
				DriveID:      firecracker.String("code"),
				PathOnHost:   firecracker.String(cfg.CodeDrivePath),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			})
		} else {
			fmt.Printf("[VM] Code drive not found: %s (err: %v)\n", cfg.CodeDrivePath, err)
		}
	} else {
		fmt.Println("[VM] No code drive path specified")
	}

	fcCfg := firecracker.Config{
		SocketPath:      cfg.SocketPath,
		KernelImagePath: cfg.KernelPath,
		KernelArgs:      bootArgs,
		Drives:          drives,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(cfg.VCPUCount),
			MemSizeMib: firecracker.Int64(cfg.MemSizeMB),
		},
	}

	if cfg.TAPDeviceName != "" {
		fcCfg.NetworkInterfaces = []firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					HostDevName: cfg.TAPDeviceName,
					MacAddress:  "AA:FC:00:00:00:01",
				},
			},
		}
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(m.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	machine, err := firecracker.NewMachine(ctx, fcCfg, firecracker.WithProcessRunner(cmd))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create machine: %w", err)
	}

	if err := machine.Start(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start machine: %w", err)
	}

	return &VM{
		Machine: machine,
		Config:  cfg,
		Ctx:     ctx,
		Cancel:  cancel,
	}, nil
}

func (v *VM) Wait() error {
	return v.Machine.Wait(v.Ctx)
}

func (v *VM) Shutdown() error {
	v.Cancel()
	return v.Machine.Shutdown(v.Ctx)
}

func (v *VM) Stop() error {
	v.Cancel()
	return v.Machine.StopVMM()
}

