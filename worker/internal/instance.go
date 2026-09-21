package internal

import (
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/network"
	"aether/shared/telemetry"
	"aether/shared/vm"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	ID         string
	FunctionID string

	// status is guarded by mu; all reads/writes go through getStatus/setStatus.
	// It must never be touched directly, because monitorVM can run concurrently
	// with the stop path.
	status          InstanceStatus
	StartedAt       time.Time
	VCPU            int64
	MemMB           int64
	vmMgr           *vm.Manager
	bridgeMgr       *network.BridgeManager
	vm              *vm.VM
	tap             *network.TAPDevice
	vmIP            string
	socketPath      string
	activeRequests  int64
	lastRequestTime time.Time
	proxyPort       int
	proxyServer     *http.Server
	// portOwned tracks whether the worker allocated a proxy port for this
	// instance that has not been released yet, so cleanup releases it once.
	portOwned bool
	// registered tracks whether the instance is currently registered in etcd,
	// so unregistration happens exactly once.
	registered   bool
	mu           sync.Mutex
	stdoutWriter *telemetry.VMLogWriter
	stderrWriter *telemetry.VMLogWriter
	span         trace.Span
	onVMDeath    func(functionID, instanceID string)
	onRequest    func(functionID string)
	netnsMgr     *network.NetnsManager
	inet         *network.InstanceNet
}

type InstanceConfig struct {
	KernelPath   string
	RuntimePath  string
	Drives       []vm.DriveSpec
	SocketPath   string
	VCPUCount    int64
	MemSizeMB    int64
	FunctionPort int
	BootToken    string
	MMDSData     map[string]interface{}
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

func (i *Instance) IdleDuration() time.Duration {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.lastRequestTime.IsZero() {
		return time.Since(i.StartedAt)
	}
	return time.Since(i.lastRequestTime)
}

func NewInstance(functionID string, vmMgr *vm.Manager, bridgeMgr *network.BridgeManager) *Instance {
	instID := id.GenerateInstanceID()
	return &Instance{
		ID:           instID,
		FunctionID:   functionID,
		status:       StatusStarting,
		StartedAt:    time.Now(),
		vmMgr:        vmMgr,
		bridgeMgr:    bridgeMgr,
		stdoutWriter: telemetry.NewVMLogWriter(functionID, instID, false),
		stderrWriter: telemetry.NewVMLogWriter(functionID, instID, true),
	}
}

func (i *Instance) Start(cfg InstanceConfig) error {
	log := logger.With("instance", i.ID, "function", i.FunctionID)
	i.setStatus(StatusStarting)

	_, span := telemetry.Tracer("aether-worker").Start(context.Background(), "instance.lifecycle",
		trace.WithAttributes(
			attribute.String("function.id", i.FunctionID),
			attribute.String("instance.id", i.ID),
		),
	)
	i.mu.Lock()
	i.span = span
	i.mu.Unlock()
	i.stdoutWriter.SetSpan(span)
	i.stderrWriter.SetSpan(span)

	var (
		inet          *network.InstanceNet
		tap           *network.TAPDevice
		tapName, vmIP string
		err           error
	)

	if i.netnsMgr != nil {
		inet, err = i.netnsMgr.Setup(i.ID)
		if err != nil {
			return fmt.Errorf("failed to set up isolated network: %w", err)
		}
		i.mu.Lock()
		i.inet = inet
		i.mu.Unlock()
		tapName, vmIP = inet.TapName, inet.GuestIP
		i.setVMIP(vmIP)
		log.Debug("isolated network ready", "ns", inet.NSName, "guest_ip", vmIP, "gateway", inet.HostIP)
	} else {
		vmIP, err = i.bridgeMgr.AllocateVMIP()
		if err != nil {
			return fmt.Errorf("failed to allocate IP: %w", err)
		}
		i.setVMIP(vmIP)
		log.Debug("allocated IP", "ip", vmIP)

		tapName = i.bridgeMgr.NextTAPName()
		tap, err = i.bridgeMgr.CreateTAPDevice(tapName)
		if err != nil {
			i.bridgeMgr.ReleaseVMIP(vmIP)
			i.setVMIP("")
			return fmt.Errorf("failed to create TAP: %w", err)
		}
		i.mu.Lock()
		i.tap = tap
		i.mu.Unlock()
		log.Debug("created TAP", "tap", tapName)

		if err := i.bridgeMgr.AttachTAPToBridge(tap.Name); err != nil {
			i.bridgeMgr.DeleteTAPDevice(tap.Name)
			i.bridgeMgr.ReleaseVMIP(vmIP)
			i.mu.Lock()
			i.tap = nil
			i.mu.Unlock()
			i.setVMIP("")
			return fmt.Errorf("failed to attach TAP: %w", err)
		}
	}

	gateway := i.bridgeMgr.GetGatewayIP()
	guestMask := ""
	netNSPath := ""
	if inet != nil {
		gateway = inet.HostIP
		guestMask = inet.GuestMask
		netNSPath = "/var/run/netns/" + inet.NSName
	}

	vmCfg := vm.Config{
		KernelPath:    cfg.KernelPath,
		RootFSPath:    cfg.RuntimePath,
		Drives:        cfg.Drives,
		SocketPath:    cfg.SocketPath,
		VCPUCount:     cfg.VCPUCount,
		MemSizeMB:     cfg.MemSizeMB,
		TAPDeviceName: tapName,
		VMIP:          vmIP,
		GatewayIP:     gateway,
		GuestMask:     guestMask,
		NetNSPath:     netNSPath,
		BootToken:     cfg.BootToken,
		MMDSData:      cfg.MMDSData,
		Stdout:        i.stdoutWriter,
		Stderr:        i.stderrWriter,
	}

	i.mu.Lock()
	i.VCPU = cfg.VCPUCount
	i.MemMB = cfg.MemSizeMB
	i.socketPath = cfg.SocketPath
	i.mu.Unlock()

	log.Debug("launching VM", "vcpu", cfg.VCPUCount, "memory_mb", cfg.MemSizeMB, "drives", len(cfg.Drives))
	vmInstance, err := i.vmMgr.Launch(vmCfg)
	if err != nil {
		i.rollbackNetwork()
		return fmt.Errorf("failed to launch VM: %w", err)
	}
	i.mu.Lock()
	i.vm = vmInstance
	i.mu.Unlock()

	log.Info("VM launched", "ip", vmIP, "tap", tapName)
	i.setStatus(StatusReady)

	// Monitor VM process - cleanup if it dies unexpectedly
	go i.monitorVM()

	return nil
}

// rollbackNetwork undoes whichever network mode was provisioned when a later
// startup stage fails, so neither bridge nor netns resources leak.
func (i *Instance) rollbackNetwork() {
	i.mu.Lock()
	inet := i.inet
	i.inet = nil
	tap := i.tap
	i.tap = nil
	vmIP := i.vmIP
	i.vmIP = ""
	i.mu.Unlock()

	if inet != nil {
		if i.netnsMgr != nil {
			i.netnsMgr.Teardown(inet)
		}
		return
	}
	if tap != nil {
		i.bridgeMgr.DeleteTAPDevice(tap.Name)
	}
	if vmIP != "" {
		i.bridgeMgr.ReleaseVMIP(vmIP)
	}
}

// WaitReady polls the guest HTTP endpoint until it responds or the timeout
// elapses. The passed ctx cancels both individual requests and the backoff
// between them, so worker shutdown aborts an in-flight readiness wait.
func (i *Instance) WaitReady(ctx context.Context, port int, timeout time.Duration) error {
	vmIP := i.GetVMIP()
	log := logger.With("function", i.FunctionID, "ip", vmIP, "port", port)
	url := fmt.Sprintf("http://%s:%d/", vmIP, port)
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)

	log.Debug("waiting for instance ready")

	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if reqErr == nil {
			resp, err := client.Do(req)
			if err == nil {
				ready := resp.StatusCode < 500
				resp.Body.Close()
				if ready {
					log.Info("instance ready")
					return nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("instance not ready after %v", timeout)
}

func (i *Instance) StartProxy(listenPort int, targetPort int) error {
	log := logger.With("function", i.FunctionID, "instance", i.ID)
	vmIP := i.GetVMIP()

	targetURL := fmt.Sprintf("http://%s:%d", vmIP, targetPort)
	log.Info("creating proxy", "listen_port", listenPort, "vm_ip", vmIP, "target_port", targetPort, "target_url", targetURL)

	proxy := NewProxy(targetURL, i)
	if proxy == nil {
		return fmt.Errorf("failed to create proxy for %s", targetURL)
	}

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", listenPort))
	if err != nil {
		return fmt.Errorf("failed to bind proxy port %d: %w", listenPort, err)
	}

	server := &http.Server{
		Handler: proxy,
	}

	i.mu.Lock()
	i.proxyPort = listenPort
	i.proxyServer = server
	i.mu.Unlock()

	go func() {
		log.Info("proxy serving", "listen_port", listenPort)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Error("proxy error", "error", err)
		}
	}()

	return nil
}

// Stop tears the instance down. It is idempotent: resources are swapped out
// under the lock before being released, so concurrent or repeated calls (the
// VM-death callback, the scaler, worker shutdown) each release a given resource
// at most once. It is also safe after a partial Start (nil vm/tap/inet/proxy).
func (i *Instance) Stop() error {
	log := logger.With("instance", i.ID, "function", i.FunctionID)
	i.setStatus(StatusStopping)

	if i.stdoutWriter != nil {
		i.stdoutWriter.Flush()
	}
	if i.stderrWriter != nil {
		i.stderrWriter.Flush()
	}

	// Swap owned resources out so a second stop, or a stop racing with a late
	// StartProxy, only tears down what has not been handled yet.
	i.mu.Lock()
	proxyServer := i.proxyServer
	i.proxyServer = nil
	vmInstance := i.vm
	i.vm = nil
	socketPath := i.socketPath
	i.socketPath = ""
	inet := i.inet
	i.inet = nil
	tap := i.tap
	i.tap = nil
	vmIP := i.vmIP
	i.vmIP = ""
	span := i.span
	i.span = nil
	i.mu.Unlock()

	if proxyServer != nil {
		log.Debug("draining proxy")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := proxyServer.Shutdown(ctx); err != nil {
			log.Warn("proxy shutdown incomplete", "error", err)
		}
	}
	if vmInstance != nil {
		log.Debug("stopping VM")
		vmInstance.Stop()
	}
	if socketPath != "" {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			log.Warn("failed to remove firecracker socket", "path", socketPath, "error", err)
		}
	}
	if inet != nil {
		log.Debug("tearing down isolated network", "ns", inet.NSName)
		if i.netnsMgr != nil {
			i.netnsMgr.Teardown(inet)
		}
	} else {
		if tap != nil {
			log.Debug("deleting TAP", "tap", tap.Name)
			i.bridgeMgr.DeleteTAPDevice(tap.Name)
		}
		if vmIP != "" {
			log.Debug("releasing IP", "ip", vmIP)
			i.bridgeMgr.ReleaseVMIP(vmIP)
		}
	}

	if span != nil {
		span.End()
	}

	i.setStatus(StatusStopped)
	log.Info("instance stopped")
	return nil
}

func (i *Instance) GetID() string {
	return i.ID
}

func (i *Instance) GetVMIP() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.vmIP
}

func (i *Instance) setVMIP(ip string) {
	i.mu.Lock()
	i.vmIP = ip
	i.mu.Unlock()
}

func (i *Instance) GetProxyPort() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.proxyPort
}

// setProxyPort records the proxy port allocated by the worker and marks it as
// owned, so cleanup releases it exactly once even if StartProxy never runs.
func (i *Instance) setProxyPort(port int) {
	i.mu.Lock()
	i.proxyPort = port
	i.portOwned = true
	i.mu.Unlock()
}

// takePortOwnership returns true exactly once for an instance whose proxy port
// has been allocated and not released.
func (i *Instance) takePortOwnership() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	owned := i.portOwned
	i.portOwned = false
	return owned
}

func (i *Instance) getStatus() InstanceStatus {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

func (i *Instance) setStatus(status InstanceStatus) {
	i.mu.Lock()
	i.status = status
	i.mu.Unlock()
}

func (i *Instance) GetStatus() InstanceStatus {
	return i.getStatus()
}

func (i *Instance) SetStatus(status InstanceStatus) {
	i.setStatus(status)
}

// isStopped reports whether the instance reached a terminal state, meaning
// provisioning must not hand it back to the worker.
func (i *Instance) isStopped() bool {
	switch i.getStatus() {
	case StatusStopping, StatusStopped, StatusError:
		return true
	default:
		return false
	}
}

func (i *Instance) markRegistered() {
	i.mu.Lock()
	i.registered = true
	i.mu.Unlock()
}

// takeRegistered returns true exactly once for an instance currently marked as
// registered in etcd, so unregistration happens at most once.
func (i *Instance) takeRegistered() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	was := i.registered
	i.registered = false
	return was
}

func (i *Instance) SetVMDeathCallback(callback func(functionID, instanceID string)) {
	i.mu.Lock()
	i.onVMDeath = callback
	i.mu.Unlock()
}

func (i *Instance) SetOnRequest(callback func(functionID string)) {
	i.mu.Lock()
	i.onRequest = callback
	i.mu.Unlock()
}

func (i *Instance) SetNetnsManager(m *network.NetnsManager) {
	i.mu.Lock()
	i.netnsMgr = m
	i.mu.Unlock()
}

func (i *Instance) noteRequest() {
	i.mu.Lock()
	cb := i.onRequest
	i.mu.Unlock()
	if cb != nil {
		cb(i.FunctionID)
	}
}

func (i *Instance) getVM() *vm.VM {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.vm
}

func (i *Instance) monitorVM() {
	log := logger.With("instance", i.ID, "function", i.FunctionID)
	log.Debug("started VM process monitor")

	vmInstance := i.getVM()
	if vmInstance == nil {
		log.Debug("no VM to monitor")
		return
	}

	err := vmInstance.Wait()

	if status := i.getStatus(); status == StatusStopping || status == StatusStopped {
		log.Debug("VM exited normally")
		return
	}

	log.Warn("VM died unexpectedly", "error", err)
	i.setStatus(StatusError)

	i.mu.Lock()
	cb := i.onVMDeath
	i.mu.Unlock()
	if cb != nil {
		log.Info("triggering cleanup callback")
		cb(i.FunctionID, i.ID)
	} else {
		log.Warn("no cleanup callback registered")
	}
}
