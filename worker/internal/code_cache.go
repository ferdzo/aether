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
}

func NewCodeCache(minio *storage.Minio, bucket, cacheDir string) *CodeCache {
	os.MkdirAll(cacheDir, 0755)
	return &CodeCache{
		minio:    minio,
		bucket:   bucket,
		cacheDir: cacheDir,
		cached:   make(map[string]string),
	}
}

func (c *CodeCache) EnsureCode(functionID string) (string, error) {
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

	logger.Info("downloading code from minio", "function", functionID, "bucket", c.bucket)
	metrics.CodeCacheMisses.Inc()
	objectPath := functionID + "/code.ext4"

	obj, err := c.minio.GetObject(c.bucket, objectPath)
	if err != nil {
		return "", fmt.Errorf("failed to get object from minio: %w", err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		return "", fmt.Errorf("failed to read object: %w", err)
	}

	if len(data) == 0 {
		return "", fmt.Errorf("code image is empty for function %s", functionID)
	}

	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write cache file: %w", err)
	}

	c.mu.Lock()
	c.cached[functionID] = localPath
	c.mu.Unlock()

	logger.Info("code cached", "function", functionID, "path", localPath, "size", len(data))
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
