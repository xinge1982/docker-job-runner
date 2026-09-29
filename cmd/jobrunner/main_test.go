package main

import (
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
