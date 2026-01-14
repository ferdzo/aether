package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"aether/gateway/functions"
	"aether/gateway/internal"
	"aether/shared/db"
	"aether/shared/logger"
	"aether/shared/storage"
	"log/slog"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
)
type Config struct {
	EtcdEndpoints []string
	RedisAddr string
	Port int
}

func main() {

	logger.Init(slog.LevelDebug, true)

	err := godotenv.Load()
	if err != nil {
		logger.Error("Error loading .env file", "error", err)
		os.Exit(1)
	}
	config := Config{
		EtcdEndpoints: strings.Split(os.Getenv("ETCD_ENDPOINTS"), ","),
		RedisAddr: os.Getenv("REDIS_ADDR"),
		Port: func() int {
			val := os.Getenv("PORT")
			if val == "" {
				val = "8080"
			}
			port, err := strconv.Atoi(val)
			if err != nil {
				port = 8080
			}
			return port
		}(),
	}

	etcdClient, err := internal.NewEtcdClient(config.EtcdEndpoints)
	if err != nil{
		logger.Error("Error creating etcd client", "error", err)
		os.Exit(1)
	}
	defer internal.CloseEtcd(etcdClient)
	redisClient, err := internal.NewRedisClient(config.RedisAddr)
	if err != nil{
		logger.Error("Error creating redis client", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

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
	dbClient, err := db.NewDB(os.Getenv("DB_PATH"))
	if err != nil {
		logger.Error("Error creating db client", "error", err)
		os.Exit(1)
	}
	defer dbClient.Close()

	if err := dbClient.Migrate(); err != nil {
		logger.Error("Error running migrations", "error", err)
		os.Exit(1)
	}

	functionsAPI := functions.NewFunctionsAPI(dbClient, minioClient, redisClient.Client())
	discovery := internal.NewDiscovery(etcdClient)
	handler := internal.NewHandler(discovery, redisClient, dbClient)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.HandleFunc("/functions/{funcID}/*", handler.Handler)
	r.Mount("/api/functions", functionsAPI.Routes())


	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		logger.Info("Shutting down...")
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%d", config.Port)
	logger.Info("Gateway starting", "addr", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		logger.Error("Server error", "error", err)
		os.Exit(1)
	}
}
