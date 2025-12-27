package internal

import (
	"aether/shared/logger"
	"context"

	redis "github.com/redis/go-redis/v9"
)

func NewRedisClient(addr string) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})

	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		return nil, err
	}

	logger.Info("connected to redis", "addr", addr)
	return client, nil
}

func CloseRedisClient(client *redis.Client) error {
	if err := client.Close(); err != nil {
		logger.Error("failed to close redis client", "error", err)
		return err
	}
	return nil
}
