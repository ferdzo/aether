package internal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"aether/shared/protocol"

	redis "github.com/redis/go-redis/v9"
)

func streamLen(t *testing.T, client *redis.Client, stream string) int64 {
	t.Helper()
	n, err := client.XLen(context.Background(), stream).Result()
	if err != nil {
		t.Fatalf("XLEN %s: %v", stream, err)
	}
	return n
}

// pendingJob adds one process job to the provision stream and delivers it to w,
// leaving it pending with a delivery count of 1.
func pendingJob(t *testing.T, w *Worker) string {
	t.Helper()
	ctx := context.Background()

	job := protocol.Job{
		JobID:     "job-hygiene",
		RequestID: "req-hygiene",
		Mode:      jobModeProcess,
		Command:   []string{"true"},
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	id, err := w.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: protocol.StreamProvision,
		Values: map[string]interface{}{"job": string(data)},
	}).Result()
	if err != nil {
		t.Fatalf("XAdd: %v", err)
	}

	if _, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    protocol.StreamGroup,
		Consumer: w.consumerName,
		Streams:  []string{protocol.StreamProvision, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("XReadGroup: %v", err)
	}
	return id
}

// forceProcessFailures makes every process job fail before the spawn, so a
// redelivery can never succeed and the entry stays pending.
func forceProcessFailures(t *testing.T) {
	t.Helper()
	prev := provisionStart
	provisionStart = func(context.Context, *Instance, InstanceConfig) error {
		return errors.New("boom")
	}
	t.Cleanup(func() { provisionStart = prev })
}

// shortStaleClaim lets a test drive the reaper without waiting 70s.
func shortStaleClaim(t *testing.T) {
	t.Helper()
	prev := staleClaimAfter
	staleClaimAfter = 20 * time.Millisecond
	t.Cleanup(func() { staleClaimAfter = prev })
}

// An entry whose processing keeps failing must be moved to the DLQ and ACKed
// once its delivery count exceeds the cap; at or below the cap it stays pending.
func TestDeliveryCapMovesExhaustedEntryToDLQ(t *testing.T) {
	w, _ := newStreamWorker(t)
	w.cfg.MaxDeliveries = 2
	ctx := context.Background()

	forceProcessFailures(t)
	shortStaleClaim(t)

	id := pendingJob(t, w)
	time.Sleep(40 * time.Millisecond)

	// delivery count 1, at the cap: pending, no DLQ entry.
	w.moveExhaustedToDLQ(ctx)
	if n := len(pendingEntries(t, w.redis)); n != 1 {
		t.Fatalf("at cap: pending = %d, want 1", n)
	}
	if n := streamLen(t, w.redis, protocol.StreamProvisionDLQ); n != 0 {
		t.Fatalf("at cap: DLQ len = %d, want 0", n)
	}

	// Reaper redelivery, delivery count 2: still at the cap, so pending.
	time.Sleep(40 * time.Millisecond)
	w.claimOnce(ctx)
	time.Sleep(40 * time.Millisecond)
	w.moveExhaustedToDLQ(ctx)
	if n := len(pendingEntries(t, w.redis)); n != 1 {
		t.Fatalf("at cap (2): pending = %d, want 1", n)
	}

	// Reaper redelivery, delivery count 3: over the cap, moved and acked.
	time.Sleep(40 * time.Millisecond)
	w.claimOnce(ctx)
	time.Sleep(40 * time.Millisecond)
	w.moveExhaustedToDLQ(ctx)

	if n := len(pendingEntries(t, w.redis)); n != 0 {
		t.Fatalf("over cap: pending = %d, want 0 (entry must be acked)", n)
	}

	msgs, err := w.redis.XRange(ctx, protocol.StreamProvisionDLQ, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange DLQ: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("DLQ entries = %d, want 1", len(msgs))
	}
	got := msgs[0].Values
	if got["original_id"] != id {
		t.Fatalf("DLQ original_id = %v, want %q", got["original_id"], id)
	}
	if got["reason"] == nil || got["reason"] == "" {
		t.Fatalf("DLQ reason missing: %v", got)
	}
	if got["deliveries"] == nil {
		t.Fatalf("DLQ deliveries missing: %v", got)
	}
	if got["job"] == nil || got["job"] == "" {
		t.Fatalf("DLQ original payload missing: %v", got)
	}
}

// A failed DLQ write must leave the entry pending (not acked) so it retries.
func TestDeliveryCapDLQWriteFailureLeavesPending(t *testing.T) {
	w, _ := newStreamWorker(t)
	w.cfg.MaxDeliveries = 1
	ctx := context.Background()

	shortStaleClaim(t)

	id := pendingJob(t, w)

	// delivery count 2 > cap 1.
	if _, err := w.redis.XClaim(ctx, &redis.XClaimArgs{
		Stream:   protocol.StreamProvision,
		Group:    protocol.StreamGroup,
		Consumer: w.consumerName,
		MinIdle:  0,
		Messages: []string{id},
	}).Result(); err != nil {
		t.Fatalf("XClaim: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	prev := dlqAdd
	dlqAdd = func(context.Context, *redis.Client, map[string]interface{}) (string, error) {
		return "", errors.New("dlq unavailable")
	}
	t.Cleanup(func() { dlqAdd = prev })

	w.moveExhaustedToDLQ(ctx)

	if n := len(pendingEntries(t, w.redis)); n != 1 {
		t.Fatalf("DLQ write failure must leave the entry pending, pending = %d", n)
	}
	if n := streamLen(t, w.redis, protocol.StreamProvisionDLQ); n != 0 {
		t.Fatalf("DLQ should be empty on write failure, len = %d", n)
	}
}

// The trim pass bounds the stream with an approximate XTRIM.
func TestTrimStreamBoundsLength(t *testing.T) {
	w, _ := newStreamWorker(t)
	ctx := context.Background()
	w.cfg.StreamMaxLen = 10

	for i := 0; i < 50; i++ {
		if _, err := w.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: protocol.StreamProvision,
			Values: map[string]interface{}{"job": "x"},
		}).Result(); err != nil {
			t.Fatalf("XAdd %d: %v", i, err)
		}
	}

	w.trimStream(ctx)

	if n := streamLen(t, w.redis, protocol.StreamProvision); n > 10 {
		t.Fatalf("stream not bounded: XLEN = %d, want <= 10", n)
	}
}

// A zero cap disables trimming entirely.
func TestTrimStreamDisabledIsNoOp(t *testing.T) {
	w, _ := newStreamWorker(t)
	ctx := context.Background()
	w.cfg.StreamMaxLen = 0

	for i := 0; i < 30; i++ {
		if _, err := w.redis.XAdd(ctx, &redis.XAddArgs{
			Stream: protocol.StreamProvision,
			Values: map[string]interface{}{"job": "x"},
		}).Result(); err != nil {
			t.Fatalf("XAdd %d: %v", i, err)
		}
	}

	w.trimStream(ctx)

	if n := streamLen(t, w.redis, protocol.StreamProvision); n != 30 {
		t.Fatalf("disabled trim must be a no-op: XLEN = %d, want 30", n)
	}
}

// A zero delivery cap disables the DLQ pass: an exhausted entry stays pending.
func TestDeliveryCapDisabledIsNoOp(t *testing.T) {
	w, _ := newStreamWorker(t)
	ctx := context.Background()
	w.cfg.MaxDeliveries = 0

	shortStaleClaim(t)
	id := pendingJob(t, w)

	for i := 0; i < 3; i++ {
		if _, err := w.redis.XClaim(ctx, &redis.XClaimArgs{
			Stream:   protocol.StreamProvision,
			Group:    protocol.StreamGroup,
			Consumer: w.consumerName,
			MinIdle:  0,
			Messages: []string{id},
		}).Result(); err != nil {
			t.Fatalf("XClaim %d: %v", i, err)
		}
	}
	time.Sleep(40 * time.Millisecond)

	w.moveExhaustedToDLQ(ctx)

	if n := len(pendingEntries(t, w.redis)); n != 1 {
		t.Fatalf("disabled cap must not dead-letter: pending = %d, want 1", n)
	}
	if n := streamLen(t, w.redis, protocol.StreamProvisionDLQ); n != 0 {
		t.Fatalf("disabled cap must not write DLQ, len = %d", n)
	}
}
