package internal

import (
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/network"
	"aether/shared/vm"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type InstanceStatus string

const (
	StatusStarting InstanceStatus = "starting"
	StatusReady    InstanceStatus = "ready"
	StatusStopping InstanceStatus = "stopping"
	StatusStopped  InstanceStatus = "stopped"
	StatusError    InstanceStatus = "error"
)

type Instance struct {
	ID              string
	FunctionID      string
	Status          InstanceStatus
	StartedAt       time.Time
	VCPU            int64
	MemMB           int64
	vmMgr           *vm.Manager
	bridgeMgr       *network.BridgeManager
	vm              *vm.VM
	tap             *network.TAPDevice
	vmIP            string
	activeRequests  int64
	lastRequestTime time.Time
	proxyPort       int
	proxyServer     *http.Server
	mu              sync.Mutex
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

func (i *Instance) IncrementActiveRequests() {
	atomic.AddInt64(&i.activeRequests, 1)
	i.mu.Lock()
	i.lastRequestTime = time.Now()
	i.mu.Unlock()
}

func (i *Instance) DecrementActiveRequests() {
	atomic.AddInt64(&i.activeRequests, -1)
}

func (i *Instance) GetActiveRequests() int64 {
	return atomic.LoadInt64(&i.activeRequests)
}

func (i* Instance) IdleDuration() time.Duration{
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastRequestTime.IsZero() {
		return time.Since(i.StartedAt)
	}
	return time.Since(i.lastRequestTime)
}

func NewInstance(functionID string, vmMgr *vm.Manager, bridgeMgr *network.BridgeManager) *Instance {
	return &Instance{
		ID:         id.GenerateInstanceID(),
		FunctionID: functionID,
		Status:     StatusStarting,
		StartedAt:  time.Now(),
		vmMgr:      vmMgr,
		bridgeMgr:  bridgeMgr,
	}
}

func (i *Instance) Start(cfg InstanceConfig) error {
	log := logger.With("instance", i.ID, "function", i.FunctionID)
	i.Status = StatusStarting

	vmIP, err := i.bridgeMgr.AllocateVMIP()
	if err != nil {
		return fmt.Errorf("failed to allocate IP: %w", err)
	}
	i.vmIP = vmIP
	log.Debug("allocated IP", "ip", vmIP)

	tapName := i.bridgeMgr.NextTAPName()
	tap, err := i.bridgeMgr.CreateTAPDevice(tapName)
	if err != nil {
		i.bridgeMgr.ReleaseVMIP(vmIP)
		return fmt.Errorf("failed to create TAP: %w", err)
	}
	i.tap = tap
	log.Debug("created TAP", "tap", tapName)

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

	i.VCPU = cfg.VCPUCount
	i.MemMB = cfg.MemSizeMB

	log.Debug("launching VM", "vcpu", cfg.VCPUCount, "memory_mb", cfg.MemSizeMB, "code_path", cfg.CodePath)
	vmInstance, err := i.vmMgr.Launch(vmCfg)
	if err != nil {
		i.bridgeMgr.DeleteTAPDevice(tap.Name)
		i.bridgeMgr.ReleaseVMIP(vmIP)
		return fmt.Errorf("failed to launch VM: %w", err)
	}
	i.vm = vmInstance

	log.Info("VM launched", "ip", vmIP, "tap", tapName)
	i.Status = StatusReady
	return nil
}

func (i *Instance) WaitReady(port int, timeout time.Duration) error {
	log := logger.With("function", i.FunctionID, "ip", i.vmIP, "port", port)
	url := fmt.Sprintf("http://%s:%d/", i.vmIP, port)
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)

	log.Debug("waiting for instance ready")

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			log.Info("instance ready")
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("instance not ready after %v", timeout)
}

func (i *Instance) StartProxy(listenPort int, targetPort int) error {
	log := logger.With("function", i.FunctionID)

	targetURL := fmt.Sprintf("http://%s:%d", i.vmIP, targetPort)
	proxy := NewProxy(targetURL, i)
	if proxy == nil {
		return fmt.Errorf("failed to create proxy for %s", targetURL)
	}

	i.proxyPort = listenPort
	i.proxyServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", listenPort),
		Handler: proxy,
	}

	go func() {
		log.Info("proxy started", "listen_port", listenPort, "target", targetURL)
		if err := i.proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("proxy error", "error", err)
		}
	}()

	return nil
}

func (i *Instance) Stop() error {
	log := logger.With("instance", i.ID, "function", i.FunctionID)
	i.Status = StatusStopping

	if i.proxyServer != nil {
		log.Debug("stopping proxy")
		i.proxyServer.Close()
	}
	if i.vm != nil {
		log.Debug("stopping VM")
		i.vm.Stop()
	}
	if i.tap != nil {
		log.Debug("deleting TAP", "tap", i.tap.Name)
		i.bridgeMgr.DeleteTAPDevice(i.tap.Name)
	}
	if i.vmIP != "" {
		log.Debug("releasing IP", "ip", i.vmIP)
		i.bridgeMgr.ReleaseVMIP(i.vmIP)
	}

	i.Status = StatusStopped
	log.Info("instance stopped")
	return nil
}

func (i *Instance) GetID() string {
	return i.ID
}

func (i *Instance) GetVMIP() string {
	return i.vmIP
}

func (i *Instance) GetProxyPort() int {
	return i.proxyPort
}

func (i *Instance) GetStatus() InstanceStatus {
	return i.Status
}

func (i *Instance) SetStatus(status InstanceStatus) {
	i.Status = status
}
