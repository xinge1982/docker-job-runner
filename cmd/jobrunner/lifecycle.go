package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

var jobStages = []string{"preview", "apply", "run"}
var lifecycleDocker = docker

type jobContainer struct {
	Name   string         `json:"Name"`
	State  ContainerState `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type CompletedJob struct {
	JobID  string                    `json:"job_id"`
	Stages map[string]ContainerState `json:"stages"`
}

// lockJob serializes uploads, starts and deletion for the same job across
// separate CLI processes. Keep lock files outside job directories so deletion
// cannot invalidate a lock held by another process.
func lockJob(root, id string) (func(), error) {
	if err := safeDirectory(root, false); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(root, ".locks")
	if err := safeDirectory(lockDir, true); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(lockDir, id+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func dockerNames() (map[string]bool, error) {
	out, err := lifecycleDocker(context.Background(), "ps", "-a", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, name := range strings.Fields(out) {
		names[name] = true
	}
	return names, nil
}

func containersFor(id string, names map[string]bool) (map[string]jobContainer, error) {
	containers := make(map[string]jobContainer)
	for _, stage := range jobStages {
		name := nameFor(id, stage)
		if !names[name] {
			continue
		}
		out, err := lifecycleDocker(context.Background(), "inspect", "--format", "{{json .}}", name)
		if err != nil {
			return nil, err
		}
		var container jobContainer
		if err := json.Unmarshal([]byte(out), &container); err != nil {
			return nil, err
		}
		labels := container.Config.Labels
		if strings.TrimPrefix(container.Name, "/") != name || labels["app"] != "jobrunner" || labels["job.id"] != id || labels["job.stage"] != stage {
			return nil, fmt.Errorf("container %q is not owned by this job", name)
		}
		containers[stage] = container
	}
	return containers, nil
}

func finished(state ContainerState) bool {
	return !state.Running && (state.Status == "exited" || state.Status == "dead")
}

// listCompletedJobs only returns directories with at least one retained
// container and no active or pending stage containers.
func listCompletedJobs(root string) ([]CompletedJob, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []CompletedJob{}, nil
	}
	if err != nil {
		return nil, err
	}
	names, err := dockerNames()
	if err != nil {
		return nil, err
	}
	jobs := make([]CompletedJob, 0)
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !validID.MatchString(id) {
			continue
		}
		containers, err := containersFor(id, names)
		if err != nil {
			return nil, err
		}
		if len(containers) == 0 {
			continue
		}
		job := CompletedJob{JobID: id, Stages: make(map[string]ContainerState)}
		complete := true
		for stage, container := range containers {
			if !finished(container.State) {
				complete = false
				break
			}
			job.Stages[stage] = container.State
		}
		if complete {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].JobID < jobs[j].JobID })
	return jobs, nil
}

// deleteJob refuses running stages and removes only verified jobrunner
// containers. If a removal fails, keep the directory so deletion can be retried.
func deleteJob(root, id string) error {
	jobDir := filepath.Join(root, id)
	dirErr := safeDirectory(jobDir, false)
	if dirErr != nil && !errors.Is(dirErr, os.ErrNotExist) {
		return dirErr
	}
	names, err := dockerNames()
	if err != nil {
		return err
	}
	containers, err := containersFor(id, names)
	if err != nil {
		return err
	}
	if errors.Is(dirErr, os.ErrNotExist) && len(containers) == 0 {
		return fmt.Errorf("unknown job %q", id)
	}
	for stage, container := range containers {
		if container.State.Running || container.State.Status == "paused" || container.State.Status == "restarting" {
			return fmt.Errorf("job %q stage %q is still active (%s)", id, stage, container.State.Status)
		}
	}
	for _, stage := range jobStages {
		if _, ok := containers[stage]; ok {
			if _, err := lifecycleDocker(context.Background(), "rm", nameFor(id, stage)); err != nil {
				return err
			}
		}
	}
	if dirErr == nil {
		if err := os.RemoveAll(jobDir); err != nil {
			return err
		}
	}
	return nil
}
