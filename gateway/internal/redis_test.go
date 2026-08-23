package internal

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"aether/shared/logger"
	"aether/shared/protocol"

	"github.com/alicebob/miniredis/v2"
)

func TestMain(m *testing.M) {
	logger.Init(slog.LevelError, false)
	os.Exit(m.Run())
}

func TestPushJobWritesStreamEntry(t *testing.T) {
	mr := miniredis.RunT(t)
	rc, err := NewRedisClient(mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisClient: %v", err)
	}
	defer rc.Close()

	job := &protocol.Job{RequestID: "r1", FunctionID: "fn-g"}
	if err := rc.PushJob(job); err != nil {
		t.Fatalf("PushJob: %v", err)
	}

	msgs, err := rc.Client().XRange(context.Background(), protocol.StreamProvision, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRange: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 stream entry, got %d", len(msgs))
	}

	raw, ok := msgs[0].Values["job"].(string)
	if !ok {
		t.Fatalf("stream entry missing job field: %v", msgs[0].Values)
	}
	var decoded protocol.Job
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("job payload is not valid JSON: %v", err)
	}
	if decoded.FunctionID != "fn-g" || decoded.RequestID != "r1" {
		t.Fatalf("payload round-trip mismatch: %+v", decoded)
	}
}
