package internal

import (
	"aether/shared/protocol"
	"context"
	"encoding/json"
	"fmt"
	"time"

	etcd "go.etcd.io/etcd/client/v3"
)

type Discovery struct {
	client *etcd.Client
}

func NewDiscovery(etcdClient *etcd.Client) *Discovery {
	return &Discovery{
		client: etcdClient,
	}
}

func (d *Discovery) GetInstances(ctx context.Context, functionID string) ([]protocol.FunctionInstance, error) {
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