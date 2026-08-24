package internal

import (
	"bytes"
	"os"
	"testing"

	"aether/shared/storage"
)

// Round-trips a runtime image through the real storage backend.
// Gated: AETHER_STORAGE_INTEGRATION_ENDPOINT must point at a live fs server.
func TestRuntimeCacheRoundTripAgainstFsServer(t *testing.T) {
	endpoint := os.Getenv("AETHER_STORAGE_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("AETHER_STORAGE_INTEGRATION_ENDPOINT not set")
	}

	m, err := storage.NewMinio(storage.MinioConfig{
		Endpoint:  endpoint,
		AccessKey: "aether-test",
		SecretKey: "aether-test",
	})
	if err != nil {
		t.Fatalf("NewMinio: %v", err)
	}
	const bucket = "runtimes-it"
	if err := m.EnsureBucket(bucket); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}

	payload := []byte("fake-ext4-image-bytes-for-test")
	if err := m.PutObject(bucket, "testrt/rootfs.ext4", payload); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	dir := t.TempDir()
	rc := NewRuntimeCache(m, bucket, dir)

	got, err := rc.Ensure("testrt")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	onDisk, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read cached runtime: %v", err)
	}
	if !bytes.Equal(payload, onDisk) {
		t.Fatalf("round-trip mismatch: sent %d bytes, got %d", len(payload), len(onDisk))
	}

	// Second Ensure must be served from memory (same path, no error).
	again, err := rc.Ensure("testrt")
	if err != nil || again != got {
		t.Fatalf("memory hit diverged: %q vs %q (%v)", again, got, err)
	}

	if err := rc.Invalidate("testrt"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("invalidated runtime file still present")
	}
}
