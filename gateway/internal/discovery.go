package internal

import (
	"aether/shared/logger"
	"aether/shared/protocol"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	etcd "go.etcd.io/etcd/client/v3"
)

type Discovery struct {
	client        *etcd.Client
	cache         map[string][]protocol.FunctionInstance
	cacheMu       sync.RWMutex
	cacheExpiry   map[string]time.Time
	cacheTTL      time.Duration
	watchCancels  map[string]context.CancelFunc
	watchCancelMu sync.Mutex
}

func NewDiscovery(etcdClient *etcd.Client) *Discovery {
	d := &Discovery{
		client:       etcdClient,
		cache:        make(map[string][]protocol.FunctionInstance),
		cacheExpiry:  make(map[string]time.Time),
		cacheTTL:     2 * time.Second,
		watchCancels: make(map[string]context.CancelFunc),
	}
	go d.cleanupExpiredCache()
	return d
}

func (d *Discovery) GetInstances(ctx context.Context, functionID string) ([]protocol.FunctionInstance, error) {
	// Try cache first
	d.cacheMu.RLock()
	if instances, ok := d.cache[functionID]; ok {
		if time.Now().Before(d.cacheExpiry[functionID]) {
			d.cacheMu.RUnlock()
			return instances, nil
		}
	}
	d.cacheMu.RUnlock()

	// Cache miss or expired - fetch from etcd
	prefix := protocol.EtcdFuncPrefix + functionID + "/instances/"
	resp, err := d.client.Get(ctx, prefix, etcd.WithPrefix())
	if err != nil {
		return nil, err
	}

	instances := make([]protocol.FunctionInstance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var inst protocol.FunctionInstance
		if err := json.Unmarshal(kv.Value, &inst); err != nil {
			continue
		}
		instances = append(instances, inst)
	}

	// Update cache
	d.cacheMu.Lock()
	d.cache[functionID] = instances
	d.cacheExpiry[functionID] = time.Now().Add(d.cacheTTL)
	d.cacheMu.Unlock()

	// Start watching this function if not already watching
	d.startWatchIfNeeded(functionID)

	return instances, nil
}

func (d *Discovery) startWatchIfNeeded(functionID string) {
	d.watchCancelMu.Lock()
	defer d.watchCancelMu.Unlock()

	if _, exists := d.watchCancels[functionID]; exists {
		return // Already watching
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.watchCancels[functionID] = cancel

	go d.watchFunction(ctx, functionID)
}

func (d *Discovery) watchFunction(ctx context.Context, functionID string) {
	prefix := protocol.EtcdFuncPrefix + functionID + "/instances/"
	watchChan := d.client.Watch(ctx, prefix, etcd.WithPrefix())

	logger.Debug("started watching function instances", "function", functionID)

	for {
		select {
		case <-ctx.Done():
			logger.Debug("stopped watching function instances", "function", functionID)
			return
		case resp, ok := <-watchChan:
			if !ok {
				return
			}

			// Fetch fresh data on any change
			freshCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			instances, err := d.fetchInstances(freshCtx, functionID)
			cancel()

			if err == nil {
				d.cacheMu.Lock()
				d.cache[functionID] = instances
				d.cacheExpiry[functionID] = time.Now().Add(d.cacheTTL)
				d.cacheMu.Unlock()
				logger.Debug("updated cache from watch", "function", functionID, "instances", len(instances))
			}

			_ = resp // Suppress unused warning
		}
	}
}

func (d *Discovery) fetchInstances(ctx context.Context, functionID string) ([]protocol.FunctionInstance, error) {
	prefix := protocol.EtcdFuncPrefix + functionID + "/instances/"
	resp, err := d.client.Get(ctx, prefix, etcd.WithPrefix())
	if err != nil {
		return nil, err
	}

	instances := make([]protocol.FunctionInstance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var inst protocol.FunctionInstance
		if err := json.Unmarshal(kv.Value, &inst); err != nil {
			continue
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (d *Discovery) cleanupExpiredCache() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		d.cacheMu.Lock()
		now := time.Now()
		for funcID, expiry := range d.cacheExpiry {
			if now.After(expiry.Add(1 * time.Minute)) {
				delete(d.cache, funcID)
				delete(d.cacheExpiry, funcID)

				// Stop watching unused functions
				d.watchCancelMu.Lock()
				if cancel, ok := d.watchCancels[funcID]; ok {
					cancel()
					delete(d.watchCancels, funcID)
				}
				d.watchCancelMu.Unlock()

				logger.Debug("cleaned up cache for inactive function", "function", funcID)
			}
		}
		d.cacheMu.Unlock()
	}
}

func (d *Discovery) GetWorkers(ctx context.Context) ([]protocol.WorkerNode, error) {
	prefix := protocol.EtcdWorkerPrefix
	resp, err := d.client.Get(ctx, prefix, etcd.WithPrefix())
	if err != nil {
		return nil, err
	}
	workers := make([]protocol.WorkerNode, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var worker protocol.WorkerNode
		if err := json.Unmarshal(kv.Value, &worker); err != nil {
			continue
		}
		workers = append(workers, worker)
	}
	return workers, nil
}

func (d *Discovery) WaitForInstance(ctx context.Context, functionID string, timeout time.Duration) (protocol.FunctionInstance, error) {
	prefix := protocol.EtcdFuncPrefix + functionID + "/instances/"

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watchChan := d.client.Watch(ctx, prefix, etcd.WithPrefix())

	for resp := range watchChan {
		for _, ev := range resp.Events {
			if ev.Type == etcd.EventTypePut {
				var inst protocol.FunctionInstance
				if err := json.Unmarshal(ev.Kv.Value, &inst); err != nil {
					continue
				}
				if inst.Status == "ready" {
					return inst, nil
				}
			}
		}
	}
	return protocol.FunctionInstance{}, fmt.Errorf("instance not found")

}
