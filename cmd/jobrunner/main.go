package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xinge1982/docker-job-runner/jobconfig"
)

type ContainerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	ExitCode   int    `json:"ExitCode"`
	OOMKilled  bool   `json:"OOMKilled"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}
type Result struct {
	JobID       string          `json:"job_id"`
	Stage       string          `json:"stage"`
	ContainerID string          `json:"container_id,omitempty"`
	State       *ContainerState `json:"container_state,omitempty"`
	Output      string          `json:"output,omitempty"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)
var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: jobrunner <start|describe|status|result|logs|wait|stop> [flags]")
	}
	cmd := args[0]
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := f.String("config", "config.json", "configuration file")
	id := f.String("id", "", "business job ID")
	stage := f.String("stage", "", "preview, apply, or run")
	taskType := f.String("type", "", "configured task type (start only)")
	input := f.String("input", "", "input JSON (preview) or approved JSON (apply)")
	params := f.String("params", "{}", "JSON object with configured command parameters (start only)")
	requestPath := f.String("request", "", "start request JSON (use - for stdin, keeps environment values out of argv)")
	tail := f.Int("tail", 200, "number of recent log lines")
	maxBytes := f.Int64("max-bytes", 32<<20, "maximum result.json size")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if cmd != "describe" && !validID.MatchString(*id) {
		return errors.New("id must contain 1-64 letters, digits, underscores or hyphens")
	}
	if cmd != "describe" && *stage != "preview" && *stage != "apply" && *stage != "run" {
		return errors.New("stage must be preview, apply, or run")
	}
	if *tail < 0 || *tail > 10000 {
		return errors.New("tail must be between 0 and 10000")
	}
	cfg, err := jobconfig.Load(*configPath)
	if err != nil {
		return err
	}
	if cmd == "describe" {
		task, err := cfg.Task(*taskType)
		if err != nil {
			return err
		}
		task, err = discoverTask(context.Background(), task)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(task)
	}
	if cfg.WorkRoot == "" {
		return errors.New("work_root is required")
	}
	root, err := filepath.Abs(cfg.WorkRoot)
	if err != nil {
		return err
	}
	hostRoot := root
	if cfg.HostWorkRoot != "" {
		if !filepath.IsAbs(cfg.HostWorkRoot) {
			return errors.New("host_work_root must be an absolute path on the Docker host")
		}
		hostRoot = filepath.Clean(cfg.HostWorkRoot)
	}
	name := "job-" + *id + "-" + *stage
	jobDir := filepath.Join(root, *id)
	hostJobDir := filepath.Join(hostRoot, *id)

	switch cmd {
	case "start":
		t, err := cfg.Task(*taskType)
		if err != nil || t.Image == "" || t.Memory == "" || t.CPUs == "" || t.Network == "" {
			return errors.New("unknown or incomplete configured task type")
		}
		if t.Mode == "direct" && *stage != "run" {
			return errors.New("direct tasks require stage run")
		}
		if t.Mode == "direct" && len(t.Command) == 0 {
			return errors.New("direct tasks require a configured command")
		}
		if (t.Mode == "" || t.Mode == "staged") && *stage == "run" {
			return errors.New("staged tasks require preview or apply")
		}
		if t.Mode != "" && t.Mode != "staged" && t.Mode != "direct" {
			return errors.New("invalid task mode")
		}
		t, err = discoverTask(context.Background(), t)
		if err != nil {
			return err
		}
		var req jobconfig.StartRequest
		if *requestPath != "" {
			if *input != "" || *params != "{}" {
				return errors.New("--request cannot be combined with --input or --params")
			}
			var reader io.Reader
			if *requestPath == "-" {
				reader = os.Stdin
			} else {
				file, err := os.Open(*requestPath)
				if err != nil {
					return err
				}
				defer file.Close()
				reader = file
			}
			b, err := io.ReadAll(io.LimitReader(reader, (32<<20)+1))
			if err != nil {
				return err
			}
			if len(b) > 32<<20 {
				return errors.New("request exceeds 32 MiB")
			}
			if err := json.Unmarshal(b, &req); err != nil {
				return fmt.Errorf("invalid start request: %w", err)
			}
		} else {
			if *input == "" || len(*params) > 16<<10 {
				return errors.New("--input is required and --params must be at most 16 KiB")
			}
			var legacy map[string]string
			if err := json.Unmarshal([]byte(*params), &legacy); err != nil || legacy == nil {
				return errors.New("--params must be a JSON object of string values")
			}
			req.Parameters = make(map[string]json.RawMessage, len(legacy))
			for k, v := range legacy {
				b, _ := json.Marshal(v)
				req.Parameters[k] = b
			}
		}
		commandArgs, err := t.FormatParameters(req.Parameters)
		if err != nil {
			return err
		}
		if err := t.ValidateEnvironment(req.Environment); err != nil {
			return err
		}
		if *stage == "apply" {
			state, err := inspect(nameFor(*id, "preview"))
			if err != nil {
				return fmt.Errorf("preview container not found: %w", err)
			}
			if state.Running || state.Status != "exited" || state.ExitCode != 0 {
				return errors.New("preview must finish successfully before apply")
			}
			if _, err := os.Stat(filepath.Join(jobDir, "preview", "output", "result.json")); err != nil {
				return fmt.Errorf("preview result missing: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Join(jobDir, *stage, "output"), 0700); err != nil {
			return err
		}
		inputDst := filepath.Join(jobDir, *stage, "input.json")
		if *requestPath != "" {
			if err := copyJSONReader(bytes.NewReader(req.Input), inputDst); err != nil {
				return err
			}
		} else if err := copyJSONExclusive(*input, inputDst); err != nil {
			return err
		}
		return startContainer(name, *id, *stage, *taskType, hostJobDir, t, commandArgs, req.Environment)
	case "status":
		state, err := inspect(name)
		if err != nil {
			return err
		}
		out, err := docker(context.Background(), "inspect", "--format", "{{.Id}}", name)
		if err != nil {
			return err
		}
		result := Result{JobID: *id, Stage: *stage, ContainerID: strings.TrimSpace(out), State: &state,
			Output: filepath.Join(jobDir, *stage, "output")}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "result":
		if *maxBytes <= 0 || *maxBytes > 64<<20 {
			return errors.New("max-bytes must be between 1 and 67108864")
		}
		state, err := inspect(name)
		if err != nil {
			return err
		}
		if state.Status != "exited" || state.ExitCode != 0 {
			return errors.New("stage has not completed successfully")
		}
		path := filepath.Join(jobDir, *stage, "output", "result.json")
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, *maxBytes+1))
		if err != nil {
			return err
		}
		if int64(len(b)) > *maxBytes {
			return errors.New("result.json exceeds max-bytes")
		}
		if !json.Valid(b) {
			return errors.New("invalid result.json")
		}
		_, err = os.Stdout.Write(b)
		return err
	case "logs":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := docker(ctx, "logs", "--timestamps", "--tail", strconv.Itoa(*tail), name)
		fmt.Print(out)
		return err
	case "wait":
		out, err := docker(context.Background(), "wait", name)
		if err != nil {
			return err
		}
		state, err := inspect(name)
		if err != nil {
			return err
		}
		fmt.Printf("exit_code=%s oom_killed=%t status=%s\n", strings.TrimSpace(out), state.OOMKilled, state.Status)
		if state.ExitCode != 0 {
			return fmt.Errorf("container exited with code %d", state.ExitCode)
		}
		return nil
	case "stop":
		_, err := docker(context.Background(), "stop", "--time", "10", name)
		return err
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func nameFor(id, stage string) string { return "job-" + id + "-" + stage }

func copyJSONExclusive(src, dst string) error {
	var input io.Reader
	if src == "-" {
		input = os.Stdin
	} else {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	}
	return copyJSONReader(input, dst)
}

func copyJSONReader(input io.Reader, dst string) error {
	b, err := io.ReadAll(io.LimitReader(input, (32<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 32<<20 {
		return errors.New("input exceeds 32 MiB")
	}
	if !json.Valid(b) {
		return errors.New("input must contain valid JSON")
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("input already exists (job/stage cannot be submitted twice): %w", err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

func containerArgs(name, id, stage, kind, hostJobDir string, t jobconfig.TaskType, commandArgs []string, environment map[string]string) ([]string, error) {
	stageDir := filepath.Join(hostJobDir, stage)
	args := []string{"create", "--name", name,
		"--label", "app=jobrunner", "--label", "job.id=" + id,
		"--label", "job.stage=" + stage, "--label", "job.type=" + kind,
		"--restart", "no", "--init", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", "--cap-drop", "ALL",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--security-opt", "no-new-privileges", "--pids-limit", "256",
		"--memory", t.Memory, "--cpus", t.CPUs, "--network", t.Network,
		"--mount", "type=bind,src=" + filepath.Join(stageDir, "input.json") + ",dst=/job/input.json,readonly",
		"--mount", "type=bind,src=" + filepath.Join(stageDir, "output") + ",dst=/job/output",
	}
	if stage == "apply" {
		args = append(args, "--mount", "type=bind,src="+filepath.Join(hostJobDir, "preview", "output")+",dst=/job/preview,readonly")
	}
	if t.WorkDir != "" {
		if !filepath.IsAbs(t.WorkDir) {
			return nil, errors.New("work_dir must be an absolute container path")
		}
		args = append(args, "--workdir", t.WorkDir)
	}
	for _, mount := range t.Mounts {
		cleanTarget := filepath.Clean(mount.Target)
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Target) || cleanTarget == "/job" || strings.HasPrefix(cleanTarget, "/job/") || strings.ContainsAny(mount.Source+mount.Target, ",\n\r") {
			return nil, fmt.Errorf("invalid mount %q -> %q", mount.Source, mount.Target)
		}
		src := filepath.Clean(mount.Source)
		option := "type=bind,src=" + src + ",dst=" + cleanTarget
		if mount.ReadOnly {
			option += ",readonly"
		}
		args = append(args, "--mount", option)
	}
	if t.EnvFile != "" {
		if !filepath.IsAbs(t.EnvFile) {
			return nil, errors.New("env_file must be an absolute path visible to jobrunner")
		}
		if _, err := os.Stat(t.EnvFile); err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		args = append(args, "--env-file", t.EnvFile)
	}
	for key, value := range t.Environment {
		if !validEnvName.MatchString(key) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("invalid environment entry %q", key)
		}
		args = append(args, "--env", key+"="+value)
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		args = append(args, "--env", name)
	}
	args = append(args, t.Image)
	if len(t.Command) == 0 {
		if len(commandArgs) != 0 {
			return nil, errors.New("command parameters require a configured command")
		}
		args = append(args, "--stage", stage, "--input", "/job/input.json", "--output", "/job/output")
	} else {
		for _, part := range t.Command {
			if part == "" || strings.ContainsRune(part, 0) {
				return nil, errors.New("command contains an empty or invalid argument")
			}
		}
		args = append(args, t.Command...)
		args = append(args, commandArgs...)
	}
	return args, nil
}

func startContainer(name, id, stage, kind, hostJobDir string, t jobconfig.TaskType, commandArgs []string, environment map[string]string) error {
	args, err := containerArgs(name, id, stage, kind, hostJobDir, t, commandArgs, environment)
	if err != nil {
		return err
	}
	idOut, err := dockerWithEnv(context.Background(), environment, args...)
	if err != nil {
		return err
	}
	if _, err := docker(context.Background(), "start", name); err != nil {
		return fmt.Errorf("container %s created (%s) but could not start: %w", name, strings.TrimSpace(idOut), err)
	}
	fmt.Printf("job_id=%s stage=%s container_id=%s\n", id, stage, strings.TrimSpace(idOut))
	return nil
}

func inspect(name string) (ContainerState, error) {
	out, err := docker(context.Background(), "inspect", "--format", "{{json .State}}", name)
	if err != nil {
		return ContainerState{}, err
	}
	var state ContainerState
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return state, err
	}
	return state, nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	return dockerWithEnv(ctx, nil, args...)
}

func dockerWithEnv(ctx context.Context, environment map[string]string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if len(environment) != 0 {
		cmd.Env = os.Environ()
		for name, value := range environment {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	// docker CLI can be installed on the host; no shell is involved.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
