package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

const maxSchemaBytes = 64 << 10

var schemaArgument = regexp.MustCompile(`^--[A-Za-z][A-Za-z0-9-]*$`)

type limitedOutput struct {
	bytes.Buffer
	max int
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("job schema output exceeded size limit")
	}
	return b.Buffer.Write(p)
}

// discoverTask never receives runtime environment values or access to a
// network. The fixed program must handle the schema argument without work.
func discoverTask(parent context.Context, task jobconfig.TaskType) (jobconfig.TaskType, error) {
	if task.SchemaArgument == "" {
		return task, nil
	}
	if !schemaArgument.MatchString(task.SchemaArgument) || task.Image == "" {
		return jobconfig.TaskType{}, errors.New("invalid schema argument or image")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return jobconfig.TaskType{}, err
	}
	name := "job-schema-" + hex.EncodeToString(suffix[:])
	args := []string{"run", "--rm", "--pull=never", "--name", name,
		"--network", "none", "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--pids-limit", "32",
		"--memory", "128m", "--cpus", "0.5",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=16m",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
	if task.WorkDir != "" {
		if !filepath.IsAbs(task.WorkDir) {
			return jobconfig.TaskType{}, errors.New("schema work_dir must be absolute")
		}
		args = append(args, "--workdir", task.WorkDir)
	}
	for _, mount := range task.Mounts {
		target := filepath.Clean(mount.Target)
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Target) ||
			target == "/job" || strings.HasPrefix(target, "/job/") ||
			strings.ContainsAny(mount.Source+mount.Target, ",\r\n") {
			return jobconfig.TaskType{}, fmt.Errorf("invalid schema mount target %q", mount.Target)
		}
		args = append(args, "--mount", "type=bind,src="+filepath.Clean(mount.Source)+
			",dst="+target+",readonly")
	}
	for _, part := range task.Command {
		if part == "" || strings.ContainsRune(part, 0) {
			return jobconfig.TaskType{}, errors.New("invalid schema command")
		}
	}
	args = append(args, task.Image)
	args = append(args, task.Command...)
	args = append(args, task.SchemaArgument)

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	stdout := &limitedOutput{max: maxSchemaBytes}
	stderr := &limitedOutput{max: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	// docker run can be interrupted after the daemon has created the container.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanupCancel()
	_, _ = docker(cleanupCtx, "rm", "-f", name)
	if err != nil {
		if ctx.Err() != nil {
			return jobconfig.TaskType{}, fmt.Errorf("job schema timed out: %w", ctx.Err())
		}
		return jobconfig.TaskType{}, fmt.Errorf("job schema command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var schema jobconfig.Schema
	if err := decoder.Decode(&schema); err != nil {
		return jobconfig.TaskType{}, fmt.Errorf("invalid job schema JSON: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		return jobconfig.TaskType{}, errors.New("job schema stdout must contain exactly one JSON document")
	}
	return task.WithSchema(schema)
}
