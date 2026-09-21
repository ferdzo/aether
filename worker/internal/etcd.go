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

// HasReadyInstance is the idempotency guard for provision-job redelivery:
// true means another consumer already fulfilled this job with a ready VM.
func (r *Registry) HasReadyInstance(functionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := r.client.Get(ctx,
		protocol.EtcdFuncPrefix+functionID+"/instances/",
		etcd.WithPrefix())
	if err != nil {
		return false, fmt.Errorf("failed to query instances: %w", err)
	}
	for _, kv := range resp.Kvs {
		var inst protocol.FunctionInstance
		if err := json.Unmarshal(kv.Value, &inst); err != nil {
			continue
		}
		if inst.Status == "ready" {
			return true, nil
		}
	}
	return false, nil
}

func (r *Registry) Close() error {
	return r.client.Close()
}

// PutJob stores a job record durably. Unlike worker/instance registrations it
// is not attached to a lease: job records must outlive the worker that wrote
// them so the API can still answer for finished jobs.
func (r *Registry) PutJob(rec protocol.JobRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal job record: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	key := protocol.JobKey(rec.JobID)
	if _, err := r.client.Put(ctx, key, string(val)); err != nil {
		return fmt.Errorf("failed to put job record: %w", err)
	}
	logger.Info("job record stored", "job_id", rec.JobID, "state", rec.State)
	return nil
}

// GetJob reads a job record by id. A missing record is an error.
func (r *Registry) GetJob(jobID string) (protocol.JobRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := r.client.Get(ctx, protocol.JobKey(jobID))
	if err != nil {
		return protocol.JobRecord{}, fmt.Errorf("failed to get job record: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return protocol.JobRecord{}, fmt.Errorf("job %s not found", jobID)
	}

	var rec protocol.JobRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &rec); err != nil {
		return protocol.JobRecord{}, fmt.Errorf("failed to unmarshal job record: %w", err)
	}
	return rec, nil
}
