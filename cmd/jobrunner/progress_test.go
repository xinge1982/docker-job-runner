package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadProgressFromDockerLogs(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$1" = "logs" ] && [ "$2" = "--timestamps" ] && [ "$3" = "job-test-run" ] || exit 2
printf '%s\n' '2026-10-01T12:00:00Z JOBRUNNER_PROGRESS {"version":1,"percent":25}'
printf '%s\n' '2026-10-01T12:01:00Z JOBRUNNER_PROGRESS {"version":1,"percent":75}' >&2
printf '%s\n' '2026-10-01T12:02:00Z ordinary output'
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p, err := readProgress("job-test-run")
	if err != nil || p == nil || p.Percent == nil || *p.Percent != 75 {
		t.Fatalf("progress=%+v error=%v", p, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := readProgress("job-test-run"); err == nil {
		t.Fatal("expected log error")
	}
}
