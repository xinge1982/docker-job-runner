package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

func TestContainerArgsDoNotContainEnvironmentValues(t *testing.T) {
	task := jobconfig.TaskType{
		Image: "alpine:3.14", Memory: "1g", CPUs: "1.0", Network: "none",
		Command: []string{"/app/tool"},
	}
	args, err := containerArgs("job-test-run", "test", "run", "test", "/tmp/test", task, nil,
		map[string]string{"POSTGRES_PASSWORD": "private-secret"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "private-secret") || !strings.Contains(joined, "--env POSTGRES_PASSWORD") {
		t.Fatalf("unexpected environment argument: %s", joined)
	}
}

func TestFormatDockerCommandRedactsEnvironmentValues(t *testing.T) {
	args := []string{
		"create",
		"--env", "POSTGRES_PASSWORD=private-secret",
		"--env", "ACCESS_TOKEN",
		"--env=INLINE_TOKEN=inline-secret",
		"-e", "SHORT_TOKEN=short-secret",
		"-eJOINED_TOKEN=joined-secret",
		"--env-file", "/secret/job.env",
		"--env-file=/secret/second.env",
		"alpine:3.14",
		"sh", "-c", "echo hello",
	}
	original := append([]string(nil), args...)
	command := formatDockerCommand(args)

	for _, secret := range []string{
		"private-secret",
		"inline-secret",
		"short-secret",
		"joined-secret",
		"/secret/job.env",
		"/secret/second.env",
	} {
		if strings.Contains(command, secret) {
			t.Fatalf("debug command exposed %q: %s", secret, command)
		}
	}
	for _, name := range []string{
		"POSTGRES_PASSWORD",
		"ACCESS_TOKEN",
		"INLINE_TOKEN",
		"SHORT_TOKEN",
		"JOINED_TOKEN",
	} {
		if !strings.Contains(command, name+"=<redacted>") {
			t.Fatalf("debug command omitted redacted variable %q: %s", name, command)
		}
	}
	if strings.Join(args, "\x00") != strings.Join(original, "\x00") {
		t.Fatalf("formatting mutated source arguments: %#v", args)
	}
}

func TestDebugDockerCommandWritesToProvidedWriter(t *testing.T) {
	var output bytes.Buffer
	debugDockerCommand(&output, []string{"start", "job-test-run"})
	if got, want := output.String(), "debug: docker command: docker start job-test-run\n"; got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

func TestWriteStartRecordContainsOnlyRedactedCommands(t *testing.T) {
	stageDir := t.TempDir()
	result := jobconfig.StartResult{
		JobID:         "test",
		Stage:         "run",
		ContainerID:   "container-id",
		ContainerName: "job-test-run",
		DockerCommands: []string{
			"docker create --env PASSWORD=<redacted> alpine:3.14",
			"docker start job-test-run",
		},
	}
	if err := writeStartRecord(stageDir, result); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(stageDir, "start.json"))
	if err != nil {
		t.Fatal(err)
	}
	var stored jobconfig.StartResult
	if err := json.Unmarshal(contents, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ContainerName != result.ContainerName || strings.Contains(string(contents), "private-secret") {
		t.Fatalf("unexpected start record: %s", contents)
	}
}
