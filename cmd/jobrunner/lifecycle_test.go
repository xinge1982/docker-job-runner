package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListCompletedAndDeleteJob(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"finished", "active", "upload_only"} {
		if err := os.Mkdir(filepath.Join(root, id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	state := map[string]ContainerState{
		"job-finished-preview": {Status: "exited", ExitCode: 0},
		"job-finished-apply":   {Status: "exited", ExitCode: 1},
		"job-active-run":       {Status: "running", Running: true},
	}
	removed := []string{}
	previous := lifecycleDocker
	lifecycleDocker = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			keys := make([]string, 0, len(state))
			for name := range state {
				keys = append(keys, name)
			}
			return strings.Join(keys, "\n"), nil
		case "inspect":
			name := args[len(args)-1]
			st, ok := state[name]
			if !ok {
				return "", errors.New("not found")
			}
			id, stage := "finished", "preview"
			if strings.HasPrefix(name, "job-active-") {
				id, stage = "active", "run"
			} else if strings.HasSuffix(name, "-apply") {
				stage = "apply"
			}
			c := jobContainer{Name: "/" + name, State: st}
			c.Config.Labels = map[string]string{"app": "jobrunner", "job.id": id, "job.stage": stage}
			b, _ := json.Marshal(c)
			return string(b), nil
		case "rm":
			name := args[len(args)-1]
			removed = append(removed, name)
			delete(state, name)
			return name, nil
		}
		return "", errors.New("unexpected Docker operation")
	}
	t.Cleanup(func() { lifecycleDocker = previous })

	jobs, err := listCompletedJobs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].JobID != "finished" || jobs[0].Stages["apply"].ExitCode != 1 {
		t.Fatalf("unexpected completed jobs: %+v", jobs)
	}
	if err := deleteJob(root, "active"); err == nil || len(removed) != 0 {
		t.Fatal("running job was deleted")
	}
	if err := deleteJob(root, "finished"); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed containers: %v", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "finished")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job directory still exists: %v", err)
	}
	if err := deleteJob(root, "upload_only"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRejectsForeignContainer(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "job1"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := lifecycleDocker
	lifecycleDocker = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "ps" {
			return "job-job1-run\n", nil
		}
		if args[0] == "inspect" {
			return `{"Name":"/job-job1-run","State":{"Status":"exited"},"Config":{"Labels":{"app":"someone-else"}}}`, nil
		}
		t.Fatal("attempted to remove foreign container")
		return "", nil
	}
	t.Cleanup(func() { lifecycleDocker = previous })
	if err := deleteJob(root, "job1"); err == nil {
		t.Fatal("accepted foreign container")
	}
	if _, err := os.Stat(filepath.Join(root, "job1")); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteKeepsFilesWhenContainerRemovalFails(t *testing.T) {
	root := t.TempDir()
	jobDir := filepath.Join(root, "job2")
	if err := os.Mkdir(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	previous := lifecycleDocker
	lifecycleDocker = func(_ context.Context, args ...string) (string, error) {
		switch args[0] {
		case "ps":
			return "job-job2-run\n", nil
		case "inspect":
			return `{"Name":"/job-job2-run","State":{"Status":"exited"},"Config":{"Labels":{"app":"jobrunner","job.id":"job2","job.stage":"run"}}}`, nil
		case "rm":
			return "", errors.New("Docker daemon unavailable")
		}
		return "", errors.New("unexpected Docker operation")
	}
	t.Cleanup(func() { lifecycleDocker = previous })
	if err := deleteJob(root, "job2"); err == nil {
		t.Fatal("expected a Docker removal error")
	}
	if _, err := os.Stat(jobDir); err != nil {
		t.Fatalf("job files were removed after Docker error: %v", err)
	}
}
