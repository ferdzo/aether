package internal

import (
	"aether/shared/logger"
	"time"

	etcd "go.etcd.io/etcd/client/v3"
)

func NewEtcdClient(endpoints []string) (*etcd.Client, error) {
	client, err := etcd.New(etcd.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

func CloseEtcd(client *etcd.Client) error {
	if err := client.Close(); err != nil {
		logger.Error("failed to close etcd client", "error", err)
		return err
	}
	return nil
}