package jobrunnerclient

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGetStatusProgressAndLegacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "runner")
	client := Client{Binary: path, Config: "/unused/config.json"}
	for _, tc := range []struct {
		name         string
		extra        string
		wantProgress bool
	}{
		{"progress", `,"progress":{"version":1,"percent":0,"phase":"prepare"},"progress_error":"partial logs"`, true},
		{"legacy", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := `{"job_id":"test","stage":"run","container_state":{"Status":"running","Running":true},"output":"/jobs/test/run/output"` + tc.extra + `}`
			script := "#!/bin/sh\nprintf '%s\\n' '" + response + "'\n"
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := client.GetStatus(context.Background(), "test", "run")
			if err != nil {
				t.Fatal(err)
			}
			if (s.Progress != nil) != tc.wantProgress {
				t.Fatalf("unexpected progress: %+v", s.Progress)
			}
			if tc.wantProgress && (s.Progress.Percent == nil || *s.Progress.Percent != 0 || s.ProgressError != "partial logs") {
				t.Fatalf("lost progress fields: %+v", s)
			}
		})
	}
}
