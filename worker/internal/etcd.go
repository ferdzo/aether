package internal

import (
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/protocol"
	"aether/shared/system"
	"context"
	"encoding/json"
	"fmt"
	"time"

	etcd "go.etcd.io/etcd/client/v3"
)

const leaseTTL = 10 // seconds

type Registry struct {
	client   *etcd.Client
	leaseID  etcd.LeaseID
	workerID string
	workerIP string
}

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

func NewRegistry(client *etcd.Client, workerIP string) *Registry {
	return &Registry{
		client:   client,
		workerID: id.GetWorkerID(),
		workerIP: workerIP,
	}
}

func (r *Registry) RegisterWorker() (func(), error) {
	ctx := context.Background()

	leaseResp, err := r.client.Grant(ctx, leaseTTL)
	if err != nil {
		return nil, fmt.Errorf("failed to grant lease: %w", err)
	}
	r.leaseID = leaseResp.ID

	sysInfo := system.GetInfo()
	info := protocol.WorkerNode{
		ID:            r.workerID,
		Hostname:      sysInfo.Hostname,
		PublicIP:      r.workerIP,
		TotalCPU:      sysInfo.CPUCount,
		TotalMemoryMB: sysInfo.MemoryMB,
		Version:       "0.1.0",
		LastHeartbeat: time.Now(),
	}

	val, _ := json.Marshal(info)
	key := protocol.WorkerKey(r.workerID)

	_, err = r.client.Put(ctx, key, string(val), etcd.WithLease(leaseResp.ID))
	if err != nil {
		return nil, fmt.Errorf("failed to register worker: %w", err)
	}

	keepAliveCh, err := r.client.KeepAlive(ctx, leaseResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to start keepalive: %w", err)
	}

	go func() {
		for range keepAliveCh {
			// Lease renewed
		}
		logger.Warn("keepalive channel closed, worker lease expired")
	}()

	logger.Info("worker registered", "worker_id", r.workerID, "lease_id", leaseResp.ID)

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r.client.Revoke(ctx, leaseResp.ID)
		logger.Info("worker deregistered", "worker_id", r.workerID)
	}, nil
}

func (r *Registry) RegisterInstance(functionID, instanceID string, proxyPort int, internalIP string) error {
	if r.leaseID == 0 {
		return fmt.Errorf("worker not registered, no lease")
	}

	info := protocol.FunctionInstance{
		InstanceID: instanceID,
		FunctionID: functionID,
		WorkerID:   r.workerID,
		HostIP:     r.workerIP,
		ProxyPort:  proxyPort,
		InternalIP: internalIP,
		Status:     "ready",
		StartedAt:  time.Now(),
	}

	val, _ := json.Marshal(info)
	key := protocol.InstanceKey(functionID, instanceID)

	_, err := r.client.Put(context.Background(), key, string(val), etcd.WithLease(r.leaseID))
	if err != nil {
		return fmt.Errorf("failed to register function: %w", err)
	}

	logger.Info("instance registered", "function", functionID, "instance", instanceID, "key", key)
	return nil
}

func (r *Registry) UnregisterInstance(functionID, instanceID string) error {
	key := protocol.InstanceKey(functionID, instanceID)
	_, err := r.client.Delete(context.Background(), key)
	if err != nil {
		return fmt.Errorf("failed to unregister instance: %w", err)
	}
	logger.Info("instance unregistered", "function", functionID)
	return nil
}

func (r *Registry) Close() error {
	return r.client.Close()
}
