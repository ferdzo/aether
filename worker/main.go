package main

import (
	"aether/shared/logger"
	"aether/worker/internal"
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger.Init(slog.LevelDebug, false)
	logger.Info("Starting Worker")
	worker := internal.NewWorker(&internal.Config{
		WorkerID:       "worker-1",
		WorkerIP:       "127.0.0.1",
		RedisAddr:      "127.0.0.1:6379",
		EtcdEndpoints:  []string{"localhost:2379"},
		FirecrackerBin: "/home/ferdzo/projects/firecracker_testing/firecracker",
		KernelPath:     "/home/ferdzo/projects/firecracker_testing/vmlinux-6.1.128",
		RuntimePath:    "/home/ferdzo/projects/aether/node-rootfs.ext4",
		FunctionsDir:   "/home/ferdzo/projects/aether/functions", // where function .ext4 files live
		SocketDir:      "/tmp/firecracker",
		BridgeName:     "fc-bridge0",
		BridgeCIDR:     "172.16.0.0/24",
		FunctionPort:   3000,
	})

	ctx, cancel := context.WithCancel(context.Background())

	// Handle shutdown signals
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
