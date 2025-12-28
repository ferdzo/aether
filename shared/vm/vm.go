package vm

import (
	"context"
	"fmt"
	"os"
	"strings"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// generateMACFromIP creates a unique MAC address based on the VM's IP
// Format: AA:FC:00:00:XX:YY where XX.YY are last two octets of IP in hex
func generateMACFromIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return "AA:FC:00:00:00:01" // fallback
	}
	var oct2, oct3 int
	fmt.Sscanf(parts[2], "%d", &oct2)
	fmt.Sscanf(parts[3], "%d", &oct3)
	return fmt.Sprintf("AA:FC:00:00:%02X:%02X", oct2, oct3)
}

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
		bootArgs = fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 init=/init ip=%s::%s:255.255.255.0::eth0:off", cfg.VMIP, cfg.GatewayIP)
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
			drives = append(drives, models.Drive{
				DriveID:      firecracker.String("code"),
				PathOnHost:   firecracker.String(cfg.CodeDrivePath),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			})
		}
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
		// Generate unique MAC from VM IP (last 2 octets)
		mac := generateMACFromIP(cfg.VMIP)
		fcCfg.NetworkInterfaces = []firecracker.NetworkInterface{
			{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					HostDevName: cfg.TAPDeviceName,
					MacAddress:  mac,
				},
			},
		}
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(m.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		// WithStdout(io.Discard).
		// WithStderr(io.Discard).
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

