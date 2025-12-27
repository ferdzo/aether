package main

import (
	"aether/shared/logger"
	"aether/worker/internal"
	"context"
	"fmt"
	"log/slog"
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
		FunctionsDir:   os.Getenv("FUNCTIONS_DIR"),
		SocketDir:      os.Getenv("SOCKET_DIR"),
		BridgeName:     os.Getenv("BRIDGE_NAME"),
		BridgeCIDR:     os.Getenv("BRIDGE_CIDR"),
		FunctionPort:   func() int {
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

	fmt.Println("creating etcd client")
	client, err := internal.NewEtcdClient(config.EtcdEndpoints)
	if err != nil {
		logger.Error("Error creating etcd client", "error", err)
		os.Exit(1)
	}
	fmt.Println("creating registry")
	registry := internal.NewRegistry(client, config.WorkerIP)
	fmt.Println("registering worker")
	unregister, err := registry.RegisterWorker()
	if err != nil {
		logger.Error("Error registering worker", "error", err)
		os.Exit(1)
	}
	defer unregister()
	defer registry.Close()


	worker := internal.NewWorker(config, registry)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {	
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down...")
		cancel()
	}()

	if err := worker.Run(ctx); err != nil {
		logger.Error("worker error", "error", err)
	}

	worker.Shutdown()
	os.Exit(0)
}
