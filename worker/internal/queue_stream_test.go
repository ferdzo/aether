package internal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aether/shared/metrics"
	"aether/shared/protocol"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	redis "github.com/redis/go-redis/v9"
)

func newStreamWorker(t *testing.T) (*Worker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	cfg := &Config{WorkerID: "cw"}
	codeCache := NewCodeCache(newDeadMinio(t), "bucket", t.TempDir())
	w := NewWorker(cfg, newDeadRegistry(t), codeCache, client)

	if err := w.ensureStreamGroup(context.Background()); err != nil {
		t.Fatalf("ensureStreamGroup: %v", err)
	}
	return w, mr
}

func pendingEntries(t *testing.T, client *redis.Client) []redis.XPendingExt {
	t.Helper()
	res, err := client.XPendingExt(context.Background(), &redis.XPendingExtArgs{
		Stream: protocol.StreamProvision,
		Group:  protocol.StreamGroup,
		Idle:   0,
		Start:  "-",
		End:    "+",
		Count:  100,
	}).Result()
	if err != nil {
		t.Fatalf("XPendingExt: %v", err)
	}
	return res
}

func TestEnsureStreamGroupIdempotent(t *testing.T) {
	w, _ := newStreamWorker(t)
	if err := w.ensureStreamGroup(context.Background()); err != nil {
		t.Fatalf("second group creation should tolerate BUSYGROUP: %v", err)
	}
}

func TestProcessMessageAcksMalformedJob(t *testing.T) {
	w, _ := newStreamWorker(t)

	before := testutil.ToFloat64(metrics.PoisonJobsTotal)
	w.processMessage(context.Background(), redis.XMessage{
		ID:     "0-1",
		Values: map[string]interface{}{"not_job": "x"},
	})

	if got := testutil.ToFloat64(metrics.PoisonJobsTotal); got != before+1 {
		t.Fatalf("PoisonJobsTotal = %f, want %f", got, before+1)
	}
	if n := len(pendingEntries(t, w.redis)); n != 0 {
		t.Fatalf("malformed job must be acked, pending = %d", n)
	}
}

func TestFailedJobStaysPendingThenIsReclaimed(t *testing.T) {
	w, _ := newStreamWorker(t)
	ctx := context.Background()

	job := protocol.Job{RequestID: "req-9", FunctionID: "fn-y", Count: 1}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: protocol.StreamProvision,
		Values: map[string]interface{}{"job": string(data)},
	}).Result(); err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	// Simulate another worker taking ownership, then dying mid-spawn.
	if _, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    protocol.StreamGroup,
		Consumer: "ghost",
		Streams:  []string{protocol.StreamProvision, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("ghost XReadGroup: %v", err)
	}

	prevAfter := staleClaimAfter
	prevEvery := staleClaimCheckEvery
	staleClaimAfter = 50 * time.Millisecond
	staleClaimCheckEvery = time.Hour // loop never runs; we drive claimOnce manually
	defer func() {
		staleClaimAfter = prevAfter
		staleClaimCheckEvery = prevEvery
	}()
	time.Sleep(120 * time.Millisecond)

	retriesBefore := testutil.ToFloat64(metrics.JobRetriesTotal)
	w.claimOnce(ctx)

	pending := pendingEntries(t, w.redis)
	if len(pending) != 1 {
		t.Fatalf("failed job must stay pending, got %d entries", len(pending))
	}
	if pending[0].Consumer != "cw" {
		t.Fatalf("entry should have been reclaimed by cw, owned by %q", pending[0].Consumer)
	}
	if pending[0].RetryCount < 1 {
		t.Fatalf("expected delivery count >= 1 after reclaim, got %d", pending[0].RetryCount)
	}
	if got := testutil.ToFloat64(metrics.JobRetriesTotal); got <= retriesBefore {
		t.Fatalf("JobRetriesTotal not incremented: %f -> %f", retriesBefore, got)
	}
}
