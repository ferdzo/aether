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
}

func NewRuntimeCache(minio *storage.Minio, bucket, cacheDir string) *RuntimeCache {
	os.MkdirAll(cacheDir, 0o755)
	return &RuntimeCache{
		minio:    minio,
		bucket:   bucket,
		cacheDir: cacheDir,
		cached:   make(map[string]string),
	}
}

// Ensure returns a local path to the rootfs for the given runtime name,
// downloading it from storage on first use.
func (c *RuntimeCache) Ensure(runtime string) (string, error) {
	if runtime == "" {
		return "", fmt.Errorf("runtime name is empty")
	}
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

	objectPath := runtime + "/rootfs.ext4"
	logger.Info("downloading runtime image", "runtime", runtime, "bucket", c.bucket)
	metrics.CodeCacheMisses.Inc()

	obj, err := c.minio.GetObject(c.bucket, objectPath)
	if err != nil {
		return "", fmt.Errorf("failed to get runtime %q: %w", runtime, err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return "", fmt.Errorf("failed to read runtime image: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("runtime image %q is empty", runtime)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return "", fmt.Errorf("failed to write runtime cache: %w", err)
	}

	c.mu.Lock()
	c.cached[runtime] = localPath
	c.mu.Unlock()

	logger.Info("runtime cached", "runtime", runtime, "path", localPath, "size", len(data))
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
