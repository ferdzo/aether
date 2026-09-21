package internal

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"aether/shared/protocol"
)

// Dead-etcd path: PutJob/GetJob must fail gracefully rather than hang or panic.
// This mirrors the existing dead-registry test pattern.
func TestRegistryJobRecordAgainstDeadEtcd(t *testing.T) {
	r := newDeadRegistry(t)

	if _, err := r.GetJob("missing"); err == nil {
		t.Fatal("GetJob against dead etcd returned no error")
	}
	if err := r.PutJob(protocol.JobRecord{JobID: "j-dead", State: protocol.JobStateDone, ExitCode: 42}); err == nil {
		t.Fatal("PutJob against dead etcd returned no error")
	}
}

// Opt-in live round-trip: set AETHER_TEST_ETCD=http://127.0.0.1:2379.
// Without it the round-trip is skipped and only the dead-etcd error paths above
// are exercised.
func TestRegistryJobRecordRoundTrip(t *testing.T) {
	endpoint := os.Getenv("AETHER_TEST_ETCD")
	if endpoint == "" {
		t.Skip("set AETHER_TEST_ETCD to run the job record etcd round-trip")
	}

	client, err := NewEtcdClient([]string{endpoint})
	if err != nil {
		t.Fatalf("NewEtcdClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	r := NewRegistry(client, "127.0.0.1")

	jobID := fmt.Sprintf("it-%d", time.Now().UnixNano())
	key := protocol.JobKey(jobID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = client.Delete(ctx, key)
	})

	now := time.Now().UTC().Truncate(time.Millisecond)
	want := protocol.JobRecord{
		JobID:      jobID,
		RequestID:  "req-it",
		Mode:       jobModeProcess,
		State:      protocol.JobStateDone,
		ExitCode:   42,
		WorkerID:   "worker-it",
		Error:      "",
		StartedAt:  now,
		FinishedAt: now.Add(2 * time.Second),
	}
	if err := r.PutJob(want); err != nil {
		t.Fatalf("PutJob: %v", err)
	}

	got, err := r.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.JobID != want.JobID || got.State != want.State || got.ExitCode != want.ExitCode ||
		got.RequestID != want.RequestID || got.Mode != want.Mode || got.WorkerID != want.WorkerID {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.FinishedAt.Equal(want.FinishedAt) {
		t.Fatalf("timestamps not preserved: got %v/%v want %v/%v",
			got.StartedAt, got.FinishedAt, want.StartedAt, want.FinishedAt)
	}
}
