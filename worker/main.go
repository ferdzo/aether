package main

import (
	"aether/shared/logger"
	"aether/shared/storage"
	"aether/worker/internal"
	"context"
	"log/slog"
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
	logger.Info("Starting Worker")
	err := godotenv.Load()
	if err != nil {
		logger.Error("Error loading .env file", "error", err)
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
	scalingCfg := internal.ScalingConfig{
		Enabled:          true,
		CheckInterval:    1 * time.Second,
		ScaleUpThreshold: 3,
		ScaleDownAfter:   30 * time.Second,
		MinInstances:     1,
		MaxInstances:     10,
		ScaleToZeroAfter: 5 * time.Minute,
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

	if err := worker.Run(ctx); err != nil {
		logger.Error("worker error", "error", err)
	}


	worker.Shutdown()
	os.Exit(0)
}
