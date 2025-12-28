package builder

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func BuildFromArchive(archiveData []byte, filename string) ([]byte, error) {
	tempDir, _ := os.MkdirTemp("", "aether-build-")
	defer os.RemoveAll(tempDir)

	extractDir := filepath.Join(tempDir, "code")
	os.MkdirAll(extractDir, 0755)

	outputPath := filepath.Join(tempDir, "code.ext4")

	archivePath := filepath.Join(tempDir, filename)
	os.WriteFile(archivePath, archiveData, 0644)

	if strings.HasSuffix(filename, ".zip") {
		extractZip(archivePath, extractDir)
	} else if strings.HasSuffix(filename, ".tar.gz") || strings.HasSuffix(filename, ".tgz") {
		extractTarGz(archivePath, extractDir)
	}

	if err := createExt4Image(extractDir, outputPath); err != nil {
		return nil, fmt.Errorf("failed to create ext4 image: %w", err)
	}
	return os.ReadFile(outputPath)
}

func createExt4Image(src, dest string) error {
	cmd := exec.Command("mke2fs", "-t", "ext4", "-d", src, dest, "2M")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mke2fs failed: %w", err)
	}
	return nil
}

func extractZip(src, dest string) error {
	r, _ := zip.OpenReader(src)
	defer r.Close()

	for _, f := range r.File {
		path := filepath.Join(dest, f.Name)
		if f.FileInfo().IsDir() {
			os.MkdirAll(path, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0755)
		outFile, _ := os.Create(path)
		rc, _ := f.Open()
		io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
	}
	return nil
}

func extractTarGz(src, dest string) error {
	f, _ := os.Open(src)
	defer f.Close()

	gzr, _ := gzip.NewReader(f)
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}

		path := filepath.Join(dest, header.Name)
		if header.Typeflag == tar.TypeDir {
			os.MkdirAll(path, 0755)
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0755)
		outFile, _ := os.Create(path)
		io.Copy(outFile, tr)
		outFile.Close()
	}
	return nil
}
