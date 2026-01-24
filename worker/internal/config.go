package internal

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
