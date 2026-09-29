package main

import (
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
	"strconv"
	"strings"
	"time"
)

// Only these task types, images and networks may be selected by a caller.
type TaskType struct {
	Image   string `json:"image"`
	Memory  string `json:"memory"`
	CPUs    string `json:"cpus"`
	Network string `json:"network"`
}
type Config struct {
	WorkRoot     string              `json:"work_root"`
	HostWorkRoot string              `json:"host_work_root"`
	Tasks        map[string]TaskType `json:"tasks"`
}
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: jobrunner <start|status|result|logs|wait|stop> [flags]")
	}
	cmd := args[0]
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	configPath := f.String("config", "config.json", "configuration file")
	id := f.String("id", "", "business job ID")
	stage := f.String("stage", "", "preview or apply")
	taskType := f.String("type", "", "configured task type (start only)")
	input := f.String("input", "", "input JSON (preview) or approved JSON (apply)")
	tail := f.Int("tail", 200, "number of recent log lines")
	maxBytes := f.Int64("max-bytes", 32<<20, "maximum result.json size")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !validID.MatchString(*id) {
		return errors.New("id must contain 1-64 letters, digits, underscores or hyphens")
	}
	if *stage != "preview" && *stage != "apply" {
		return errors.New("stage must be preview or apply")
	}
	if *tail < 0 || *tail > 10000 {
		return errors.New("tail must be between 0 and 10000")
	}
	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
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
		if *input == "" {
			return errors.New("--input is required")
		}
		t, ok := cfg.Tasks[*taskType]
		if !ok || t.Image == "" || t.Memory == "" || t.CPUs == "" || t.Network == "" {
			return errors.New("unknown or incomplete configured task type")
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
		if err := copyJSONExclusive(*input, inputDst); err != nil {
			return err
		}
		return startContainer(name, *id, *stage, *taskType, hostJobDir, t)
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

func startContainer(name, id, stage, kind, hostJobDir string, t TaskType) error {
	stageDir := filepath.Join(hostJobDir, stage)
	args := []string{"create", "--name", name,
		"--label", "app=jobrunner", "--label", "job.id=" + id,
		"--label", "job.stage=" + stage, "--label", "job.type=" + kind,
		"--restart", "no", "--init", "--read-only", "--cap-drop", "ALL",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--security-opt", "no-new-privileges", "--pids-limit", "256",
		"--memory", t.Memory, "--cpus", t.CPUs, "--network", t.Network,
		"--mount", "type=bind,src=" + filepath.Join(stageDir, "input.json") + ",dst=/job/input.json,readonly",
		"--mount", "type=bind,src=" + filepath.Join(stageDir, "output") + ",dst=/job/output",
	}
	if stage == "apply" {
		args = append(args, "--mount", "type=bind,src="+filepath.Join(hostJobDir, "preview", "output")+",dst=/job/preview,readonly")
	}
	args = append(args, t.Image, "--stage", stage, "--input", "/job/input.json", "--output", "/job/output")
	idOut, err := docker(context.Background(), args...)
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
	cmd := exec.CommandContext(ctx, "docker", args...)
	// docker CLI can be installed on the host; no shell is involved.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
