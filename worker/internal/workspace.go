package internal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aether/shared/logger"
	"aether/shared/protocol"
)

// workspaceImageSuffix identifies workspace images in WorkspaceDir. GC only
// ever touches files with this suffix.
const workspaceImageSuffix = ".ext4"

// CreateWorkspace builds an empty ext4 image for a job at
// <dir>/<jobID>.ext4 and returns its absolute-ish host path.
//
// The image is built unprivileged with `mke2fs -d <empty staging dir>`, the
// same trick the code-image and job-rootfs builders use, so no loop mount and
// no root are needed. It is created *before* the VM launches because a declared
// drive that does not exist is a hard VM launch error.
//
// It refuses to reuse an existing image for jobID: a stale file would silently
// carry another run's data. Callers that need to replace one must GC or delete
// it first. A partially built file is removed on any failure so a retry starts
// clean.
func CreateWorkspace(dir, jobID string, sizeMB int) (string, error) {
	if err := validateWorkspaceRequest(jobID, sizeMB); err != nil {
		return "", err
	}
	if dir == "" {
		return "", errors.New("workspace directory is empty")
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create workspace directory %q: %w", dir, err)
	}

	path := filepath.Join(dir, jobID+workspaceImageSuffix)
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("workspace %q already exists; refusing to reuse it", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to stat workspace %q: %w", path, err)
	}

	// mke2fs -d needs a source directory; an empty one yields an empty
	// filesystem (no lost+found and no copied content).
	staging, err := os.MkdirTemp("", "aether-workspace-*")
	if err != nil {
		return "", fmt.Errorf("failed to create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	cmd := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-m", "0", "-d", staging, path, fmt.Sprintf("%dM", sizeMB))
	if out, err := cmd.CombinedOutput(); err != nil {
		// Remove the partial image so a retry is not blocked by the
		// "already exists" guard.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("failed to remove partial workspace image", "path", path, "error", rmErr)
		}
		return "", fmt.Errorf("mke2fs failed for workspace %q: %w: %s", path, err, strings.TrimSpace(string(out)))
	}

	return path, nil
}

// validateWorkspaceRequest rejects sizes and ids that must never reach mke2fs
// or the filesystem.
func validateWorkspaceRequest(jobID string, sizeMB int) error {
	if jobID == "" {
		return errors.New("workspace job id is empty")
	}
	// Keep the id a single path element so a crafted job id cannot escape dir.
	if filepath.Base(jobID) != jobID || jobID == "." || jobID == ".." || strings.ContainsRune(jobID, filepath.Separator) {
		return fmt.Errorf("workspace job id %q is not a safe file name", jobID)
	}
	if sizeMB <= 0 {
		return fmt.Errorf("workspace size %d MB must be positive", sizeMB)
	}
	if sizeMB > protocol.MaxWorkspaceMB {
		return fmt.Errorf("workspace size %d MB exceeds the %d MB maximum", sizeMB, protocol.MaxWorkspaceMB)
	}
	return nil
}

// GCWorkspaces removes workspace images in dir older than ttl and returns how
// many were deleted. It is best-effort: a missing directory is a no-op and a
// removal failure does not stop the sweep (the caller is expected to log, not
// fail startup). ttl <= 0 disables GC entirely.
func GCWorkspaces(dir string, ttl time.Duration) (removed int, err error) {
	if ttl <= 0 || dir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read workspace dir %q: %w", dir, err)
	}

	cutoff := time.Now().Add(-ttl)
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), workspaceImageSuffix) {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			errs = append(errs, fmt.Errorf("stat %q: %w", entry.Name(), statErr))
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if rmErr := os.Remove(filepath.Join(dir, entry.Name())); rmErr != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", entry.Name(), rmErr))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
