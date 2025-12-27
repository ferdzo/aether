package internal

import (
	"aether/shared/network"
	"aether/shared/vm"
	"fmt"
	"net/http"
	"time"
)

type Instance struct {
	FunctionID string
	vmMgr      *vm.Manager
	bridgeMgr  *network.BridgeManager
	vm         *vm.VM
	tap        *network.TAPDevice
	vmIP       string
	proxyPort  int
}

type InstanceConfig struct {
	KernelPath   string
	RuntimePath  string
	CodePath     string
	SocketPath   string
	VCPUCount    int64
	MemSizeMB    int64
	FunctionPort int
}

func NewInstance(functionID string, vmMgr *vm.Manager, bridgeMgr *network.BridgeManager) *Instance {
	return &Instance{
		FunctionID: functionID,
		vmMgr:      vmMgr,
		bridgeMgr:  bridgeMgr,
	}
}

func (i *Instance) Start(cfg InstanceConfig) error {
	// Allocate IP
	vmIP, err := i.bridgeMgr.AllocateVMIP()
	if err != nil {
		return fmt.Errorf("failed to allocate IP: %w", err)
	}
	i.vmIP = vmIP

	// Create TAP device
	tapName := i.bridgeMgr.NextTAPName()
	tap, err := i.bridgeMgr.CreateTAPDevice(tapName)
	if err != nil {
		i.bridgeMgr.ReleaseVMIP(vmIP)
		return fmt.Errorf("failed to create TAP: %w", err)
	}
	i.tap = tap

	// Attach TAP to bridge
	if err := i.bridgeMgr.AttachTAPToBridge(tap.Name); err != nil {
		i.bridgeMgr.DeleteTAPDevice(tap.Name)
		i.bridgeMgr.ReleaseVMIP(vmIP)
		return fmt.Errorf("failed to attach TAP: %w", err)
	}

	vmCfg := vm.Config{
		KernelPath:    cfg.KernelPath,
		RootFSPath:    cfg.RuntimePath,
		CodeDrivePath: cfg.CodePath,
		SocketPath:    cfg.SocketPath,
		VCPUCount:     cfg.VCPUCount,
		MemSizeMB:     cfg.MemSizeMB,
		TAPDeviceName: tap.Name,
		VMIP:          vmIP,
		GatewayIP:     i.bridgeMgr.GetGatewayIP(),
	}

	vmInstance, err := i.vmMgr.Launch(vmCfg)
	if err != nil {
		i.bridgeMgr.DeleteTAPDevice(tap.Name)
		i.bridgeMgr.ReleaseVMIP(vmIP)
		return fmt.Errorf("failed to launch VM: %w", err)
	}
	i.vm = vmInstance

	return nil
}

func (i *Instance) WaitReady(port int, timeout time.Duration) error {
	url := fmt.Sprintf("http://%s:%d/", i.vmIP, port)
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("instance not ready after %v", timeout)
}

func (i *Instance) Stop() error {
	if i.vm != nil {
		i.vm.Stop()
	}
	if i.tap != nil {
		i.bridgeMgr.DeleteTAPDevice(i.tap.Name)
	}
	if i.vmIP != "" {
		i.bridgeMgr.ReleaseVMIP(i.vmIP)
	}
	return nil
}

func (i *Instance) GetVMIP() string {
	return i.vmIP
}

func (i *Instance) GetProxyPort() int {
	return i.proxyPort
}

func (i *Instance) SetProxyPort(port int) {
	i.proxyPort = port
}
