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
	// NoNetwork selects the offline NET_MODE=none path: instances boot without
	// a NIC, so no bridge/netns setup and no CAP_NET_ADMIN are required. main.go
	// populates it from the environment.
	NoNetwork bool
	// MaxDeliveries caps how many times a provision entry may be delivered
	// before it is copied to the DLQ and ACKed. 0 disables the cap. main.go
	// defaults it to 5; a zero value (e.g. in tests) leaves the cap off.
	MaxDeliveries int
	// StreamMaxLen bounds the provision stream with an approximate XTRIM. 0
	// disables trimming. main.go defaults it to 1000; the DLQ is never trimmed.
	StreamMaxLen int64
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
