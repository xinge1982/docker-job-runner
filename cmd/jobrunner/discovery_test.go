package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

func TestDiscoverTaskUsesIsolatedSchemaCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args")
	dockerPath := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$SCHEMA_TEST_ARGS\"\n" +
		"if [ \"$1\" = run ]; then\n" +
		"  echo '{\"version\":1,\"parameters\":[{\"name\":\"count\",\"flag\":\"--count\",\"type\":\"integer\",\"required\":true}],\"environment_variables\":[{\"name\":\"POSTGRES_HOST\",\"required\":true}]}'\n" +
		"fi\n"
	if err := os.WriteFile(dockerPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SCHEMA_TEST_ARGS", logPath)
	task := jobconfig.TaskType{
		Image: "alpine:3.14", Network: "production",
		Command: []string{"/app/tool", "action"}, SchemaArgument: "--jobrunner-schema",
		Mounts: []jobconfig.TaskMount{{Source: "/srv/programs", Target: "/app/programs"}},
	}
	got, err := discoverTask(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parameters) != 1 || got.Parameters[0].Name != "count" ||
		len(got.EnvironmentVariables) != 1 || got.EnvironmentVariables[0].Name != "POSTGRES_HOST" {
		t.Fatalf("unexpected discovered task: %+v", got)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(b)
	for _, wanted := range []string{"--network\nnone\n", "readonly\n", "--jobrunner-schema\n"} {
		if !strings.Contains(args, wanted) {
			t.Fatalf("missing %q in discovery invocation: %s", wanted, args)
		}
	}
	if strings.Contains(args, "production") {
		t.Fatalf("production network was exposed during discovery: %s", args)
	}
}
