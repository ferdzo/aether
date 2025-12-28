package internal

import (
	"context"
	"encoding/json"

	"aether/shared/logger"
	"aether/shared/protocol"

	redis "github.com/redis/go-redis/v9"
)

type RedisClient struct {
	client *redis.Client
}

func NewRedisClient(addr string) ( *RedisClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
	})

	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		return nil, err
	}

	logger.Info("connected to redis", "addr", addr)
	return &RedisClient{client: client}, nil
}	

func (r *RedisClient) Close() error {
	if err := r.client.Close(); err != nil {
		logger.Error("failed to close redis client", "error", err)
		return err
	}
	return nil
}

func (r *RedisClient) PushJob(job *protocol.Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return r.client.LPush(context.Background(), protocol.QueueVMProvision, data).Err()
}
