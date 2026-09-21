package internal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"aether/shared/logger"
	"aether/shared/metrics"
	"aether/shared/storage"
)

type CodeCache struct {
	minio    *storage.Minio
	bucket   string
	cacheDir string
	mu       sync.RWMutex
	cached   map[string]string // functionID -> local path
	locks    *keyedMutex
}

func NewCodeCache(minio *storage.Minio, bucket, cacheDir string) *CodeCache {
	os.MkdirAll(cacheDir, 0755)
	return &CodeCache{
		minio:    minio,
		bucket:   bucket,
		cacheDir: cacheDir,
		cached:   make(map[string]string),
		locks:    newKeyedMutex(),
	}
}

func (c *CodeCache) EnsureCode(functionID string) (string, error) {
	return c.ensure(functionID, func() (io.ReadCloser, error) {
		return c.minio.GetObject(c.bucket, functionID+"/code.ext4")
	})
}

// ensure resolves the local image for functionID, downloading it through open
// when it is not already in memory or on disk. Concurrent calls for the same
// key are serialized so the object is fetched and published only once.
func (c *CodeCache) ensure(functionID string, open func() (io.ReadCloser, error)) (string, error) {
	localPath := filepath.Join(c.cacheDir, functionID+".ext4")

	c.mu.RLock()
	if path, ok := c.cached[functionID]; ok {
		c.mu.RUnlock()
		metrics.CodeCacheHits.Inc()
		return path, nil
	}
	c.mu.RUnlock()

	if _, err := os.Stat(localPath); err == nil {
		c.mu.Lock()
		c.cached[functionID] = localPath
		c.mu.Unlock()
		logger.Debug("code cache hit (disk)", "function", functionID)
		metrics.CodeCacheHits.Inc()
		return localPath, nil
	}

	// One download per key: losers of this lock wait, then re-check the cache
	// and disk before doing any work of their own.
	lock := c.locks.lock(functionID)
	defer c.locks.unlock(lock)

	c.mu.RLock()
	if path, ok := c.cached[functionID]; ok {
		c.mu.RUnlock()
		metrics.CodeCacheHits.Inc()
		return path, nil
	}
	c.mu.RUnlock()

	if _, err := os.Stat(localPath); err == nil {
		c.mu.Lock()
		c.cached[functionID] = localPath
		c.mu.Unlock()
		logger.Debug("code cache hit (disk)", "function", functionID)
		metrics.CodeCacheHits.Inc()
		return localPath, nil
	}

	logger.Info("downloading code from minio", "function", functionID, "bucket", c.bucket)
	metrics.CodeCacheMisses.Inc()

	obj, err := open()
	if err != nil {
		return "", fmt.Errorf("failed to get object from minio: %w", err)
	}
	defer obj.Close()

	size, err := publishAtomic(localPath, obj)
	if err != nil {
		return "", fmt.Errorf("failed to cache code image: %w", err)
	}
	if size == 0 {
		return "", fmt.Errorf("code image is empty for function %s", functionID)
	}

	c.mu.Lock()
	c.cached[functionID] = localPath
	c.mu.Unlock()

	logger.Info("code cached", "function", functionID, "path", localPath, "size", size)
	return localPath, nil
}

func (c *CodeCache) Invalidate(functionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	localPath := filepath.Join(c.cacheDir, functionID+".ext4")
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove cached file: %w", err)
	}

	delete(c.cached, functionID)
	logger.Info("code cache invalidated", "function", functionID)
	return nil
}

func (c *CodeCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		os.Remove(filepath.Join(c.cacheDir, entry.Name()))
	}

	c.cached = make(map[string]string)
	logger.Info("code cache cleared")
	return nil
}

// keyedMutex provides per-key mutual exclusion, so only one goroutine performs
// work (a download) for a given key at a time. Safe for concurrent use; the
// lock map grows with the number of distinct keys ever seen.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// lock acquires and returns the mutex for key.
func (k *keyedMutex) lock(key string) *sync.Mutex {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m
}

// unlock releases a mutex previously returned by lock.
func (k *keyedMutex) unlock(m *sync.Mutex) {
	// Intentionally left without a map lookup: the map entry stays alive for
	// the lifetime of the cache, matching the bounded set of keys.
	m.Unlock()
}

// publishAtomic streams r into a temp file in the destination directory, fsyncs
// and closes it, then atomically renames it onto localPath. The destination is
// never left partially written: on any error the temp file is removed and
// localPath is left untouched. An empty source is not published; it returns a
// zero size so callers can report a cache-specific "empty image" error.
func publishAtomic(localPath string, r io.Reader) (int64, error) {
	dir := filepath.Dir(localPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(localPath)+".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}

	n, err := io.Copy(tmp, r)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("copy object: %w", err)
	}
	if n == 0 {
		cleanup()
		return 0, nil
	}
	if err := tmp.Chmod(0o644); err != nil {
		cleanup()
		return 0, fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return 0, fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("publish cache file: %w", err)
	}
	return n, nil
}
