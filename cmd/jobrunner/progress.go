package main

import (
	"context"
	"os/exec"
	"time"

	"github.com/xinge1982/docker-job-runner/jobprogress"
)

// readProgress scans retained Docker logs with bounded memory. No background
// process is required, and progress does not depend on the HTTP client's lifetime.
func readProgress(name string) (*jobprogress.Progress, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "logs", "--timestamps", name)
	var collector jobprogress.Collector
	// A shared writer preserves complete lines when os/exec copies both streams.
	cmd.Stdout, cmd.Stderr = &collector, &collector
	err := cmd.Run()
	return collector.Latest(), err
}
