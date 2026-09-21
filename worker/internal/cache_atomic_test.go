package internal

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errAfterReader yields all of buf, then returns err on the next Read.
type errAfterReader struct {
	buf *bytes.Reader
	err error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	n, _ := r.buf.Read(p)
	if n > 0 {
		return n, nil
	}
	return 0, r.err
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover temp file %q in %s", e.Name(), dir)
		}
	}
}

func TestPublishAtomicWritesFullContentAndNoTempFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "img.ext4")
	payload := bytes.Repeat([]byte("abcdefgh"), 4096)

	n, err := publishAtomic(dst, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("publishAtomic: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("published %d bytes, want %d", n, len(payload))
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", info.Mode().Perm())
	}
	assertNoTempFiles(t, dir)
}

func TestPublishAtomicFailedCopyDoesNotPublishTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "img.ext4")
	copyErr := errors.New("source blew up")
	src := io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte("x"), 128)),
		&errAfterReader{buf: bytes.NewReader(nil), err: copyErr},
	)

	if _, err := publishAtomic(dst, src); !errors.Is(err, copyErr) {
		t.Fatalf("publishAtomic error = %v, want %v", err, copyErr)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("truncated destination was published: stat err = %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestPublishAtomicEmptySourceDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "img.ext4")

	n, err := publishAtomic(dst, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("publishAtomic: %v", err)
	}
	if n != 0 {
		t.Fatalf("published %d bytes for empty source, want 0", n)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("empty source published a file: stat err = %v", err)
	}
	assertNoTempFiles(t, dir)
}

// countingSource returns a fresh opener that counts how many times the source
// was actually opened and sleeps briefly to widen any race window.
func countingSource(t *testing.T, payload []byte, calls *atomic.Int32) func() (io.ReadCloser, error) {
	t.Helper()
	return func() (io.ReadCloser, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
}

func TestCodeCacheEnsureDownloadsOncePerKey(t *testing.T) {
	dir := t.TempDir()
	cc := NewCodeCache(nil, "bucket", dir) // minio is unused: ensure is injected directly.
	payload := bytes.Repeat([]byte("code-image"), 512)
	var calls atomic.Int32
	open := countingSource(t, payload, &calls)

	const goroutines = 16
	want := filepath.Join(dir, "fn-1.ext4")
	results := make([]string, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = cc.ensure("fn-1", open)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != want {
			t.Fatalf("goroutine %d: path = %q, want %q", i, results[i], want)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("source opened %d times, want 1", got)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("cached content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	assertNoTempFiles(t, dir)
}

func TestRuntimeCacheEnsureDownloadsOncePerKey(t *testing.T) {
	dir := t.TempDir()
	rc := NewRuntimeCache(nil, "bucket", dir) // minio is unused: ensure is injected directly.
	payload := bytes.Repeat([]byte("rootfs"), 512)
	var calls atomic.Int32
	open := countingSource(t, payload, &calls)

	const goroutines = 16
	want := filepath.Join(dir, "python.ext4")
	results := make([]string, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = rc.ensure("python", open)
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != want {
			t.Fatalf("goroutine %d: path = %q, want %q", i, results[i], want)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("source opened %d times, want 1", got)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("cached content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	assertNoTempFiles(t, dir)
}

// Distinct keys must still populate independently (the lock is keyed, not global).
func TestCodeCacheEnsureDifferentKeysDownloadIndependently(t *testing.T) {
	dir := t.TempDir()
	cc := NewCodeCache(nil, "bucket", dir)
	var calls atomic.Int32
	open := func() (io.ReadCloser, error) {
		calls.Add(1)
		return io.NopCloser(bytes.NewReader([]byte("img"))), nil
	}
	for _, key := range []string{"a", "b"} {
		if _, err := cc.ensure(key, open); err != nil {
			t.Fatalf("ensure(%q): %v", key, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("source opened %d times, want 2", got)
	}
}
