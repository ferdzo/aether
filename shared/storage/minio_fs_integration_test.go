package storage

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"testing"
)

// TestMinioWrapperAgainstFsServer verifies Aether's exact storage surface
// (EnsureBucket -> PutObject -> GetObject) against a live ferdzo/fs server.
//
// Gated: runs only when AETHER_STORAGE_INTEGRATION_ENDPOINT is set, e.g.
//
//	AETHER_STORAGE_INTEGRATION_ENDPOINT=127.0.0.1:2600 go test ./storage/ -run Integration -v
func TestMinioWrapperAgainstFsServer(t *testing.T) {
	endpoint := os.Getenv("AETHER_STORAGE_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("AETHER_STORAGE_INTEGRATION_ENDPOINT not set")
	}

	m, err := NewMinio(MinioConfig{
		Endpoint:  endpoint,
		AccessKey: "aether-test",
		SecretKey: "aether-test",
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("NewMinio: %v", err)
	}

	bucket := "function-code-it"

	if err := m.EnsureBucket(bucket); err != nil {
		t.Fatalf("EnsureBucket: %v", err)
	}
	if err := m.EnsureBucket(bucket); err != nil {
		t.Fatalf("EnsureBucket idempotent call: %v", err)
	}

	payload := make([]byte, 1<<20) // 1 MiB, matches real code-image scale
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := m.PutObject(bucket, "it/function-x/code.ext4", payload); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	obj, err := m.GetObject(bucket, "it/function-x/code.ext4")
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer obj.Close()
	got, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read object body: %v", err)
	}
	if !bytes.Equal(payload, got) {
		t.Fatalf("round-trip mismatch: sent %d bytes, got %d", len(payload), len(got))
	}

	// GetObject is lazy in minio-go: a missing key errors on first Read.
	missing, err := m.GetObject(bucket, "it/does-not-exist.ext4")
	if err != nil {
		t.Fatalf("GetObject (lazy) returned early error: %v", err)
	}
	defer missing.Close()
	if _, err := io.ReadAll(missing); err == nil {
		t.Fatal("expected error reading missing key, got nil")
	}
}
