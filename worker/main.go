package main

import (
	"aether/shared/logger"
	"aether/shared/metrics"
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

	codeCache := internal.NewCodeCache(minioClient, config.MinioBucket, config.CodeCacheDir)

	redisClient, err := internal.NewRedisClient(config.RedisAddr)
	if err != nil {
		logger.Error("Error creating redis client", "error", err)
		os.Exit(1)
	}
	defer internal.CloseRedisClient(redisClient)

	worker := internal.NewWorker(config, registry, codeCache, redisClient)
	reaper := internal.NewReaper(worker)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down...")
		cancel()
	}()
	go reaper.Run(ctx)
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
