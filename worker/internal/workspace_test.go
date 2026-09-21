package internal

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aether/shared/protocol"
)

// ext4SuperblockMagic is the little-endian magic at byte offset 0x438 of an
// ext2/3/4 filesystem. Checking it keeps the test independent of debugfs.
const ext4SuperblockMagic = 0xEF53

func readExt4Magic(t *testing.T, path string) uint16 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()

	buf := make([]byte, 2)
	if _, err := f.ReadAt(buf, 0x438); err != nil {
		t.Fatalf("read superblock: %v", err)
	}
	return binary.LittleEndian.Uint16(buf)
}

func TestCreateWorkspaceBuildsExt4Image(t *testing.T) {
	dir := t.TempDir()

	path, err := CreateWorkspace(dir, "job-ws-1", 8)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	want := filepath.Join(dir, "job-ws-1.ext4")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	if info.Size() != 8*1024*1024 {
		t.Fatalf("size = %d, want %d", info.Size(), 8*1024*1024)
	}
	if magic := readExt4Magic(t, path); magic != ext4SuperblockMagic {
		t.Fatalf("superblock magic = 0x%04X, want ext4 0x%04X", magic, ext4SuperblockMagic)
	}
}

func TestCreateWorkspaceRefusesDuplicateID(t *testing.T) {
	dir := t.TempDir()

	if _, err := CreateWorkspace(dir, "job-dup", 8); err != nil {
		t.Fatalf("first CreateWorkspace: %v", err)
	}
	_, err := CreateWorkspace(dir, "job-dup", 8)
	if err == nil {
		t.Fatal("second CreateWorkspace for the same id must fail rather than reuse the image")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateWorkspaceRejectsBadSizes(t *testing.T) {
	dir := t.TempDir()

	for _, size := range []int{0, -1, protocol.MaxWorkspaceMB + 1} {
		if _, err := CreateWorkspace(dir, "job-size", size); err == nil {
			t.Fatalf("size %d must be rejected", size)
		}
	}
	// Nothing may have been created for the rejected requests.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected sizes left files behind: %v", entries)
	}
}

func TestCreateWorkspaceRejectsUnsafeJobID(t *testing.T) {
	dir := t.TempDir()

	for _, id := range []string{"", "../escape", "a/b", ".", ".."} {
		if _, err := CreateWorkspace(dir, id, 8); err == nil {
			t.Fatalf("job id %q must be rejected", id)
		}
	}
}

// mke2fs leaving a partial file behind must not block a retry: CreateWorkspace
// removes whatever it created when the build fails.
func TestCreateWorkspaceRemovesPartialFileOnFailure(t *testing.T) {
	// A fake mke2fs that records the image it was told to build, creates it,
	// then fails — mimicking a build interrupted after the file was made.
	// mke2fs is invoked as "... <image> <size>M", so the image is the
	// second-to-last argument.
	bin := t.TempDir()
	marker := filepath.Join(bin, "invoked-with")
	fake := filepath.Join(bin, "mke2fs")
	script := "#!/bin/sh\n" +
		"eval \"image=\\${$(($# - 1))}\"\n" +
		"printf '%s' \"$image\" > \"$WS_TEST_MARKER\"\n" +
		": > \"$image\"\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake mke2fs: %v", err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("WS_TEST_MARKER", marker)

	dir := t.TempDir()
	_, err := CreateWorkspace(dir, "job-partial", 8)
	if err == nil {
		t.Fatal("CreateWorkspace must fail when mke2fs fails")
	}

	// The fake must have been asked to build the real image, not some other
	// path, or this test would prove nothing.
	wantImage := filepath.Join(dir, "job-partial.ext4")
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != wantImage {
		t.Fatalf("fake mke2fs invoked with %q (err %v), want %q", got, readErr, wantImage)
	}
	if _, statErr := os.Stat(wantImage); !os.IsNotExist(statErr) {
		t.Fatalf("partial image not removed: stat err = %v", statErr)
	}
}

func TestGCWorkspacesRemovesOldKeepsNew(t *testing.T) {
	dir := t.TempDir()

	old := filepath.Join(dir, "old.ext4")
	newer := filepath.Join(dir, "new.ext4")
	other := filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, newer, other} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	// Age only old.ext4 and the non-image file beyond the TTL.
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("Chtimes %s: %v", p, err)
		}
	}

	removed, err := GCWorkspaces(dir, time.Hour)
	if err != nil {
		t.Fatalf("GCWorkspaces: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old.ext4 still present: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("new.ext4 was removed: %v", err)
	}
	// GC must never touch non-image files, however old.
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-image file was removed: %v", err)
	}
}

func TestGCWorkspacesNoop(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.ext4")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// ttl <= 0 disables GC.
	if removed, err := GCWorkspaces(dir, 0); err != nil || removed != 0 {
		t.Fatalf("ttl=0: removed=%d err=%v, want 0/nil", removed, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("ttl=0 removed a file: %v", err)
	}

	// A missing directory is a no-op, not an error.
	removed, err := GCWorkspaces(filepath.Join(dir, "does-not-exist"), time.Hour)
	if err != nil || removed != 0 {
		t.Fatalf("missing dir: removed=%d err=%v, want 0/nil", removed, err)
	}
}
