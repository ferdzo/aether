package main

import (
	"aether/shared/logger"
	"aether/shared/metrics"
	"aether/shared/network"
	"aether/shared/storage"
	"aether/shared/telemetry"
	"aether/worker/internal"
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

func readEnv() *internal.Config {
	return &internal.Config{
		WorkerID:       os.Getenv("WORKER_ID"),
		WorkerIP:       os.Getenv("WORKER_IP"),
		RedisAddr:      os.Getenv("REDIS_ADDR"),
		EtcdEndpoints:  strings.Split(os.Getenv("ETCD_ENDPOINTS"), ","),
		FirecrackerBin: os.Getenv("FIRECRACKER_BIN"),
		KernelPath:     os.Getenv("KERNEL_PATH"),
		RuntimePath:    os.Getenv("RUNTIME_PATH"),
		CodeCacheDir:   os.Getenv("CODE_CACHE_DIR"),
		SocketDir:      os.Getenv("SOCKET_DIR"),
		BridgeName:     os.Getenv("BRIDGE_NAME"),
		BridgeCIDR:     os.Getenv("BRIDGE_CIDR"),
		MinioBucket:    os.Getenv("MINIO_BUCKET"),
		GuestDNS:       parseGuestDNS(os.Getenv("GUEST_DNS")),
		FunctionPort: func() int {
			val := os.Getenv("FUNCTION_PORT")
			if val == "" {
				val = "3000"
			}
			port, err := strconv.Atoi(val)
			if err != nil {
				port = 3000
			}
			return port
		}(),
	}
}

// defaultGuestDNS is used when GUEST_DNS is unset. The host's own resolver
// (systemd-resolved on 127.0.0.53) is not reachable from a guest, so we default
// to public resolvers rather than copying the host's resolv.conf.
var defaultGuestDNS = []string{"1.1.1.1", "8.8.8.8"}

// parseGuestDNS parses a comma-separated GUEST_DNS list, falling back to
// defaultGuestDNS when it is empty or contains no usable entry.
func parseGuestDNS(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultGuestDNS
	}
	dns := make([]string, 0, 2)
	for _, part := range strings.Split(raw, ",") {
		if entry := strings.TrimSpace(part); entry != "" {
			dns = append(dns, entry)
		}
	}
	if len(dns) == 0 {
		return defaultGuestDNS
	}
	return dns
}

// setupEgressNAT installs host-side NAT for whichever network mode is active,
// using the host default interface. bridgeName is empty in netns mode, where
// the rules are scoped by subnet instead.
func setupEgressNAT(mode, bridgeName, cidr string) {
	extIface, err := network.GetDefaultInterface()
	if err != nil {
		logger.Warn("egress NAT unavailable: no default interface detected", "mode", mode, "error", err)
		return
	}

	nat := network.NewBridgeManager(bridgeName, cidr)
	if err := nat.SetupNAT(extIface); err != nil {
		logger.Warn("egress NAT setup failed; functions remain isolated", "mode", mode, "error", err)
		return
	}
	logger.Info("egress NAT configured", "mode", mode, "external_iface", extIface)
}

func main() {
	logger.Init(slog.LevelDebug, false)
	metrics.Init()
	logger.Info("Starting Worker")
	err := godotenv.Load()
	if err != nil {
		logger.Error("Error loading .env file", "error", err)
	}

	shutdownTelemetry, err := telemetry.Init(context.Background(), telemetry.Config{
		ServiceName:  "aether-worker",
		OTLPEndpoint: os.Getenv("OTLP_ENDPOINT"),
		Enabled:      os.Getenv("OTLP_ENDPOINT") != "",
	})
	if err != nil {
		logger.Warn("telemetry init failed", "error", err)
	} else {
		defer shutdownTelemetry(context.Background())
	}

	config := readEnv()

	client, err := internal.NewEtcdClient(config.EtcdEndpoints)
	if err != nil {
		logger.Error("Error creating etcd client", "error", err)
		os.Exit(1)
	}
	registry := internal.NewRegistry(client, config.WorkerIP)
	unregister, err := registry.RegisterWorker()
	if err != nil {
		logger.Error("Error registering worker", "error", err)
		os.Exit(1)
	}
	defer unregister()
	defer registry.Close()

	minioConfig := storage.MinioConfig{
		Endpoint:  os.Getenv("MINIO_ENDPOINT"),
		AccessKey: os.Getenv("MINIO_ACCESS_KEY"),
		SecretKey: os.Getenv("MINIO_SECRET_KEY"),
	}

	minioClient, err := storage.NewMinio(minioConfig)
	if err != nil {
		logger.Error("Error creating minio client", "error", err)
		os.Exit(1)
	}

	if config.MinioBucket == "" {
		config.MinioBucket = "function-code"
	}
	if config.CodeCacheDir == "" {
		config.CodeCacheDir = "/var/aether/cache"
	}
	if config.SocketDir == "" {
		config.SocketDir = "/tmp/firecracker"
	}
	if err := os.MkdirAll(config.SocketDir, 0o755); err != nil {
		logger.Error("Error creating firecracker socket dir", "error", err)
		os.Exit(1)
	}

	codeCache := internal.NewCodeCache(minioClient, config.MinioBucket, config.CodeCacheDir)

	runtimesDir := os.Getenv("RUNTIMES_CACHE_DIR")
	if runtimesDir == "" {
		runtimesDir = "/var/aether/runtimes"
	}
	if err := minioClient.EnsureBucket("runtimes"); err != nil {
		logger.Error("Error ensuring runtimes bucket", "error", err)
		os.Exit(1)
	}
	runtimeCache := internal.NewRuntimeCache(minioClient, "runtimes", runtimesDir)

	redisClient, err := internal.NewRedisClient(config.RedisAddr)
	if err != nil {
		logger.Error("Error creating redis client", "error", err)
		os.Exit(1)
	}
	defer internal.CloseRedisClient(redisClient)

	worker := internal.NewWorker(config, registry, codeCache, redisClient)
	worker.SetRuntimeCache(runtimeCache)

	networkMode := os.Getenv("NET_MODE")
	if networkMode == "" {
		networkMode = "bridge"
	}
	if networkMode == "netns" {
		supernet := os.Getenv("NETNS_SUPERNET")
		if supernet == "" {
			supernet = "172.31.0.0/16"
		}
		netnsMgr, err := network.NewNetnsManager(supernet)
		if err != nil {
			logger.Error("Error creating netns manager", "error", err)
			os.Exit(1)
		}
		worker.SetNetnsManager(netnsMgr)

		setupEgressNAT("netns", "", supernet)
		logger.Info("network mode: netns", "supernet", supernet)
	} else {
		setupEgressNAT("bridge", config.BridgeName, config.BridgeCIDR)
		logger.Info("network mode: bridge", "bridge", config.BridgeName, "cidr", config.BridgeCIDR)
	}
	scalingCfg := internal.ScalingConfig{
		Enabled:          true,
		CheckInterval:    1 * time.Second,
		ScaleUpThreshold: 3,
		ScaleDownAfter:   30 * time.Second,
		MinInstances:     1,
		MaxInstances:     10,
		ScaleToZeroAfter: 5 * time.Minute,
		WarmWindow:       10 * time.Minute,
	}
	scaler := internal.NewScaler(worker, &scalingCfg)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down...")
		cancel()
	}()
	go scaler.Run(ctx)
	go worker.WatchCodeUpdates(ctx)

	// Start metrics HTTP server
	go func() {
		http.Handle("/metrics", metrics.Handler())
		logger.Info("metrics server listening", "addr", ":9090")
		if err := http.ListenAndServe(":9090", nil); err != nil {
			logger.Error("metrics server error", "error", err)
		}
	}()

	if err := worker.Run(ctx); err != nil {
		logger.Error("worker error", "error", err)
	}

	worker.Shutdown()
	os.Exit(0)
}
