package jobrunnerclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

func TestEncodeStartRequest(t *testing.T) {
	request := jobconfig.StartRequest{
		Input: json.RawMessage(`{"source":"test"}`),
		Environment: map[string]string{
			"PASSWORD": "private-secret",
		},
	}
	encoded, err := encodeStartRequest("job01", "run", request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded jobconfig.StartRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Environment["PASSWORD"] != "private-secret" {
		t.Fatal("start request environment was not preserved on stdin")
	}
}

func TestStartRunRequestDetailed(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "fake-jobrunner")
	script := `#!/bin/sh
printf '%s\n' '{"job_id":"job01","stage":"run","container_id":"container-id","container_name":"job-job01-run","docker_commands":["docker create --env PASSWORD=<redacted> alpine:3.14","docker start job-job01-run"]}'
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{
		Binary: binary,
		Config: "config.json",
	}
	result, err := client.StartRunRequestDetailed(context.Background(), "job01", "example", jobconfig.StartRequest{
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ContainerID != "container-id" || result.ContainerName != "job-job01-run" || len(result.DockerCommands) != 2 {
		t.Fatalf("unexpected detailed start result: %#v", result)
	}
}
