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
}

type ScalingConfig struct {
	Enabled          bool
	CheckInterval    time.Duration
	ScaleUpThreshold int
	ScaleDownAfter   time.Duration
	MinInstances     int
	MaxInstances     int
	ScaleToZeroAfter time.Duration
}