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

// RuntimeCache resolves guest rootfs images by runtime name: memory ->
// on-disk -> fs storage download. Mirrors CodeCache so workers stay
// stateless across machines.
type RuntimeCache struct {
	minio    *storage.Minio
	bucket   string
	cacheDir string
	mu       sync.RWMutex
	cached   map[string]string // runtime name -> local rootfs path
	locks    *keyedMutex
}

func NewRuntimeCache(minio *storage.Minio, bucket, cacheDir string) *RuntimeCache {
	os.MkdirAll(cacheDir, 0o755)
	return &RuntimeCache{
		minio:    minio,
		bucket:   bucket,
		cacheDir: cacheDir,
		cached:   make(map[string]string),
		locks:    newKeyedMutex(),
	}
}

// Ensure returns a local path to the rootfs for the given runtime name,
// downloading it from storage on first use.
func (c *RuntimeCache) Ensure(runtime string) (string, error) {
	if runtime == "" {
		return "", fmt.Errorf("runtime name is empty")
	}
	objectPath := runtime + "/rootfs.ext4"
	return c.ensure(runtime, func() (io.ReadCloser, error) {
		return c.minio.GetObject(c.bucket, objectPath)
	})
}

// ensure resolves the local rootfs for runtime, downloading it through open
// when it is not already in memory or on disk. Concurrent calls for the same
// key are serialized so the object is fetched and published only once.
func (c *RuntimeCache) ensure(runtime string, open func() (io.ReadCloser, error)) (string, error) {
	localPath := filepath.Join(c.cacheDir, runtime+".ext4")

	c.mu.RLock()
	if path, ok := c.cached[runtime]; ok {
		c.mu.RUnlock()
		metrics.CodeCacheHits.Inc()
		return path, nil
	}
	c.mu.RUnlock()

	if _, err := os.Stat(localPath); err == nil {
		c.mu.Lock()
		c.cached[runtime] = localPath
		c.mu.Unlock()
		logger.Debug("runtime cache hit (disk)", "runtime", runtime)
		metrics.CodeCacheHits.Inc()
		return localPath, nil
	}

	// One download per key: losers of this lock wait, then re-check the cache
	// and disk before doing any work of their own.
	lock := c.locks.lock(runtime)
	defer c.locks.unlock(lock)

	c.mu.RLock()
	if path, ok := c.cached[runtime]; ok {
		c.mu.RUnlock()
		metrics.CodeCacheHits.Inc()
		return path, nil
	}
	c.mu.RUnlock()

	if _, err := os.Stat(localPath); err == nil {
		c.mu.Lock()
		c.cached[runtime] = localPath
		c.mu.Unlock()
		logger.Debug("runtime cache hit (disk)", "runtime", runtime)
		metrics.CodeCacheHits.Inc()
		return localPath, nil
	}

	logger.Info("downloading runtime image", "runtime", runtime, "bucket", c.bucket)
	metrics.CodeCacheMisses.Inc()

	obj, err := open()
	if err != nil {
		return "", fmt.Errorf("failed to get runtime %q: %w", runtime, err)
	}
	defer obj.Close()

	size, err := publishAtomic(localPath, obj)
	if err != nil {
		return "", fmt.Errorf("failed to cache runtime image: %w", err)
	}
	if size == 0 {
		return "", fmt.Errorf("runtime image %q is empty", runtime)
	}

	c.mu.Lock()
	c.cached[runtime] = localPath
	c.mu.Unlock()

	logger.Info("runtime cached", "runtime", runtime, "path", localPath, "size", size)
	return localPath, nil
}

// Invalidate drops a runtime from the in-memory index and disk.
func (c *RuntimeCache) Invalidate(runtime string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	localPath := filepath.Join(c.cacheDir, runtime+".ext4")
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove cached runtime: %w", err)
	}
	delete(c.cached, runtime)
	return nil
}
