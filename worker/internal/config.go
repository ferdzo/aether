package internal

import "time"

type Config struct {
	WorkerID       string
	WorkerIP       string
	RedisAddr      string
	EtcdEndpoints  []string
	FirecrackerBin string
	KernelPath     string
	RuntimePath    string
	CodeCacheDir   string
	SocketDir      string
	BridgeName     string
	BridgeCIDR     string
	FunctionPort   int
	MinioBucket    string
	// GuestDNS is injected into the guest MMDS payload as "dns" when non-empty.
	// main.go populates it from the environment.
	GuestDNS []string
}

type ScalingConfig struct {
	Enabled          bool
	CheckInterval    time.Duration
	ScaleUpThreshold int
	ScaleDownAfter   time.Duration
	MinInstances     int
	MaxInstances     int
	ScaleToZeroAfter time.Duration
	WarmWindow       time.Duration // while invoked within this window, a function never drops below MinInstances
}
