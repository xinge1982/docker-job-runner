package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

func TestUploadAndArchive(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "job123")
	content := []byte(`{"type":"FeatureCollection","features":[]}`)
	hash := sha256.Sum256(content)
	var response bytes.Buffer
	if err := uploadFile(jobDir, "roads.geojson", int64(len(content)), hex.EncodeToString(hash[:]), bytes.NewReader(content), &response); err != nil {
		t.Fatal(err)
	}
	var uploaded UploadResult
	if err := json.Unmarshal(response.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	if uploaded.Path != "/job/files/roads.geojson" || uploaded.Size != int64(len(content)) {
		t.Fatalf("unexpected upload response: %+v", uploaded)
	}
	if err := os.MkdirAll(filepath.Join(jobDir, "preview", "output"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "preview", "output", "result.json"), []byte(`{"ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := archiveJob(jobDir, "job123", &archive); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&archive)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[header.Name] = string(b)
		}
	}
	if files["job123/files/roads.geojson"] != string(content) || files["job123/preview/output/result.json"] != `{"ok":true}` {
		t.Fatalf("archive missing files: %v", files)
	}
	if err := uploadFile(jobDir, "roads.geojson", int64(len(content)), "", bytes.NewReader(content), io.Discard); err == nil {
		t.Fatal("existing upload was replaced")
	}
}

func TestUploadRejectsInvalidTransfers(t *testing.T) {
	for _, tc := range []struct {
		name, data, checksum string
		size                 int64
	}{
		{"../escape", "abc", "", 3},
		{"file.txt", "abc", "", 2},
		{"file.txt", "abc", "", 4},
		{"file.txt", "abc", strings.Repeat("0", 64), 3},
	} {
		dir := filepath.Join(t.TempDir(), "job")
		if err := uploadFile(dir, tc.name, tc.size, tc.checksum, strings.NewReader(tc.data), io.Discard); err == nil {
			t.Fatalf("accepted invalid upload %+v", tc)
		}
	}
}

func TestArchiveRejectsSymlinks(t *testing.T) {
	jobDir := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(jobDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := archiveJob(jobDir, "job", io.Discard); err == nil {
		t.Fatal("archived symlink")
	}
}

func TestJobFilesMount(t *testing.T) {
	args, err := containerArgs("job-a-run", "a", "run", "test", "/srv/jobrunner/jobs/a", taskForFilesTest(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "src=/srv/jobrunner/jobs/a/files,dst=/job/files,readonly") {
		t.Fatalf("missing read-only job file mount: %v", args)
	}
}

func taskForFilesTest() jobconfig.TaskType {
	return jobconfig.TaskType{Image: "alpine:3.14", Memory: "1g", CPUs: "1.0", Network: "none", Command: []string{"/app/tool"}}
}
