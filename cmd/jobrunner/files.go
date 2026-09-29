package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const maxUploadBytes int64 = 8 << 30
const maxArchiveBytes int64 = 16 << 30

var validFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type UploadResult struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

func safeDirectory(dir string, create bool) error {
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is not a directory", dir)
	}
	return nil
}

// uploadFile streams stdin to a temporary file, then publishes it only after
// its exact byte count and optional checksum have been verified.
func uploadFile(jobDir, name string, size int64, expected string, source io.Reader, response io.Writer) error {
	if !validFileName.MatchString(name) {
		return errors.New("invalid file name")
	}
	if size < 0 || size > maxUploadBytes {
		return errors.New("upload size is outside the allowed range")
	}
	if expected != "" {
		decoded, err := hex.DecodeString(expected)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New("sha256 must be a 64-character hex digest")
		}
	}
	if err := safeDirectory(jobDir, true); err != nil {
		return err
	}
	filesDir := filepath.Join(jobDir, "files")
	if err := safeDirectory(filesDir, true); err != nil {
		return err
	}
	incomingDir := filepath.Join(jobDir, ".incoming")
	if err := safeDirectory(incomingDir, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(incomingDir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	n, err := io.CopyN(io.MultiWriter(tmp, hash), source, size+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != size {
		return fmt.Errorf("upload size mismatch: received %d, expected %d", n, size)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if expected != "" && !strings.EqualFold(expected, actual) {
		return errors.New("upload sha256 mismatch")
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link refuses to replace an existing upload, including one created by
	// another concurrent transfer. Both names reside on the same filesystem.
	if err := os.Link(tmp.Name(), filepath.Join(filesDir, name)); err != nil {
		return fmt.Errorf("publish upload (file may already exist): %w", err)
	}
	return json.NewEncoder(response).Encode(UploadResult{Name: name, Size: size, SHA256: actual, Path: "/job/files/" + name})
}

// archiveJob writes a single job instance as a gzip-compressed tar stream.
// Symlinks and special files are rejected instead of being dereferenced.
func archiveJob(jobDir, id string, output io.Writer) error {
	if err := safeDirectory(jobDir, false); err != nil {
		return err
	}
	gz := gzip.NewWriter(output)
	tw := tar.NewWriter(gz)
	var total int64
	err := filepath.WalkDir(jobDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == filepath.Join(jobDir, ".incoming") {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("archive contains unsupported file %q", path)
		}
		if info.Mode().IsRegular() {
			if info.Size() > maxArchiveBytes-total {
				return errors.New("job archive exceeds 16 GiB")
			}
			total += info.Size()
		}
		rel, err := filepath.Rel(jobDir, path)
		if err != nil {
			return err
		}
		name := id
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		if entry.IsDir() {
			name += "/"
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.CopyN(tw, file, info.Size())
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// saveStageLogs saves Docker's combined stdout and stderr when a job is
// inspected for archiving. Existing log snapshots remain if Docker is gone.
func saveStageLogs(jobDir, id string) {
	for _, stage := range []string{"preview", "apply", "run"} {
		stageDir := filepath.Join(jobDir, stage)
		if err := safeDirectory(stageDir, false); err != nil {
			continue
		}
		logsDir := filepath.Join(stageDir, "logs")
		if err := safeDirectory(logsDir, true); err != nil {
			continue
		}
		file, err := os.CreateTemp(logsDir, ".docker-logs-*")
		if err != nil {
			continue
		}
		cmd := exec.Command("docker", "logs", "--timestamps", nameFor(id, stage))
		cmd.Stdout, cmd.Stderr = file, file
		runErr := cmd.Run()
		closeErr := file.Close()
		if runErr == nil && closeErr == nil {
			if err := os.Rename(file.Name(), filepath.Join(logsDir, "docker.log")); err == nil {
				continue
			}
		}
		os.Remove(file.Name())
	}
}
