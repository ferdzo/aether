package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

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

// DriveSpec describes an additional block device attached to the guest in
// order after the root filesystem.
type DriveSpec struct {
	Path     string
	ReadOnly bool
}

type Config struct {
	KernelPath     string
	RootFSPath     string
	RootFSReadOnly bool // zero value keeps the root filesystem read-write
	Drives         []DriveSpec
	SocketPath     string
	VCPUCount      int64
	MemSizeMB      int64
	TAPDeviceName  string
	VMIP           string
	GatewayIP      string
	GuestMask      string // defaults to 255.255.255.0; /30 networks pass 255.255.255.252
	NetNSPath      string // when set, the VM process runs inside this named netns
	BootToken      string
	MMDSData       map[string]interface{}
	Stdout         io.Writer
	Stderr         io.Writer
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

// buildDrives assembles the ordered device list for a VM. The root filesystem
// is always the first (root) device, so the guest sees it as /dev/vda; each
// configured DriveSpec follows in slice order as /dev/vdb, /dev/vdc, and so on.
// Every declared drive must have a non-empty path that exists on the host: a
// missing drive is a hard error rather than a silent skip, because the guest
// fails hard when it cannot mount a drive it was told to expect.
func buildDrives(cfg Config) ([]models.Drive, error) {
	drives := []models.Drive{
		{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(cfg.RootFSPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(cfg.RootFSReadOnly),
		},
	}

	for i, spec := range cfg.Drives {
		driveID := fmt.Sprintf("drive%d", i)
		if spec.Path == "" {
			return nil, fmt.Errorf("%s: empty path", driveID)
		}
		if _, err := os.Stat(spec.Path); err != nil {
			return nil, fmt.Errorf("%s: path %q: %w", driveID, spec.Path, err)
		}
		drives = append(drives, models.Drive{
			DriveID:      firecracker.String(driveID),
			PathOnHost:   firecracker.String(spec.Path),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(spec.ReadOnly),
		})
	}

	return drives, nil
}

func (m *Manager) Launch(cfg Config) (*VM, error) {
	if _, err := os.Stat(cfg.KernelPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("kernel not found: %s", cfg.KernelPath)
	}
	if _, err := os.Stat(cfg.RootFSPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("rootfs not found: %s", cfg.RootFSPath)
	}

	drives, err := buildDrives(cfg)
	if err != nil {
		return nil, err
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

	guestMask := cfg.GuestMask
	if guestMask == "" {
		guestMask = "255.255.255.0"
	}
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/init"
	if cfg.VMIP != "" && cfg.GatewayIP != "" {
		bootArgs = fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 init=/init ip=%s::%s:%s::eth0:off", cfg.VMIP, cfg.GatewayIP, guestMask)
	}
	if cfg.BootToken != "" {
		bootArgs = fmt.Sprintf("%s aether_token=%s", bootArgs, cfg.BootToken)
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

	if cfg.MMDSData != nil {
		fcCfg.MmdsVersion = firecracker.MMDSv1
		fcCfg.MmdsAddress = net.ParseIP("169.254.169.254")
	}

	if cfg.NetNSPath != "" {
		fcCfg.NetNS = cfg.NetNSPath
	}

	if cfg.TAPDeviceName != "" {
		mac := generateMACFromIP(cfg.VMIP)
		networkInterface := firecracker.NetworkInterface{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				HostDevName: cfg.TAPDeviceName,
				MacAddress:  mac,
			},
		}
		if cfg.MMDSData != nil {
			networkInterface.AllowMMDS = true
		}
		fcCfg.NetworkInterfaces = []firecracker.NetworkInterface{networkInterface}
	}

	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(m.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(stdout).
		WithStderr(stderr).
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

	if cfg.MMDSData != nil {
		if err := machine.SetMetadata(ctx, cfg.MMDSData); err != nil {
			machine.StopVMM()
			cancel()
			return nil, fmt.Errorf("failed to set MMDS metadata: %w", err)
		}
	}

	return &VM{
		Machine: machine,
		Config:  cfg,
		Ctx:     ctx,
		Cancel:  cancel,
	}, nil
}

// Wait blocks until the Firecracker process exits or until the VM lifetime
// context (VM.Ctx) is cancelled. Unlike the management calls below it is meant
// to observe the lifetime context, so it keeps using VM.Ctx.
func (v *VM) Wait() error {
	if v.Machine == nil {
		return fmt.Errorf("vm: machine not started")
	}
	return v.Machine.Wait(v.Ctx)
}

// Shutdown asks the guest to power off (CtrlAltDel). It must not use VM.Ctx:
// cancelling the lifetime context is itself part of teardown, and an already
// cancelled context would abort the request before it reached the VMM. A fresh,
// bounded context is used so a hung VMM cannot block shutdown forever.
//
// The lifetime context is only cancelled if the graceful request fails, as a
// fallback that makes the SDK SIGTERM the VMM. Cancelling is idempotent, so
// calling Shutdown repeatedly is safe.
func (v *VM) Shutdown() error {
	if v.Machine == nil {
		if v.Cancel != nil {
			v.Cancel()
		}
		return fmt.Errorf("vm: machine not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := v.Machine.Shutdown(ctx); err != nil {
		if v.Cancel != nil {
			v.Cancel()
		}
		return err
	}
	return nil
}

// Stop cancels the VM lifetime context (which makes the SDK stop the VMM
// process) and then signals the process directly. It is safe to call more than
// once: context cancel funcs and the SDK's StopVMM are both idempotent.
func (v *VM) Stop() error {
	if v.Cancel != nil {
		v.Cancel()
	}
	if v.Machine == nil {
		return fmt.Errorf("vm: machine not started")
	}
	return v.Machine.StopVMM()
}
