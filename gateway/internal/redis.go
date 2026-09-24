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

func NewRedisClient(addr string) (*RedisClient, error) {
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
	return r.client.XAdd(context.Background(), &redis.XAddArgs{
		Stream: protocol.StreamProvision,
		Values: map[string]interface{}{"job": string(data)},
	}).Err()
}

func (r *RedisClient) Client() *redis.Client {
	return r.client
}

// PublishJobCancel asks whichever worker owns jobID to cancel it. It reuses the
// client that publishes the provision stream; cancellation is best-effort
// pub/sub with no retry, so a worker that is down or the wrong owner simply
// misses the message.
func (r *RedisClient) PublishJobCancel(ctx context.Context, jobID string) error {
	return r.client.Publish(ctx, protocol.ChannelJobCancel, jobID).Err()
}
