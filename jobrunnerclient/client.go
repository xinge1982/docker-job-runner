// Package jobrunnerclient connects a Go application to the jobrunner CLI.
// The CLI returns as soon as the child container has started.
package jobrunnerclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Binary string
	Config string
	// Remote is an SSH destination such as "jobrunner@example.com".
	// Leave it empty to execute the CLI on the same machine.
	Remote    string
	SSHPort   int
	SSHBinary string
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

type Status struct {
	JobID       string         `json:"job_id"`
	Stage       string         `json:"stage"`
	ContainerID string         `json:"container_id"`
	State       ContainerState `json:"container_state"`
	Output      string         `json:"output"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func NewJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (c Client) StartPreview(ctx context.Context, id, taskType string, params json.RawMessage) (string, error) {
	return c.start(ctx, id, taskType, "preview", params)
}

func (c Client) StartApply(ctx context.Context, id, taskType string, approved json.RawMessage) (string, error) {
	return c.start(ctx, id, taskType, "apply", approved)
}

func (c Client) start(ctx context.Context, id, taskType, stage string, input json.RawMessage) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid job ID")
	}
	if !json.Valid(input) {
		return "", errors.New("input must be valid JSON")
	}
	if len(input) > 32<<20 {
		return "", errors.New("input exceeds 32 MiB")
	}
	out, err := c.callWithInput(ctx, input, "start", "--id", id, "--type", taskType,
		"--stage", stage, "--input", "-")
	if err != nil {
		return "", err
	}
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "container_id=") {
			return strings.TrimPrefix(field, "container_id="), nil
		}
	}
	return "", fmt.Errorf("container started but CLI response contained no container_id: %q", out)
}

func (c Client) GetStatus(ctx context.Context, id, stage string) (Status, error) {
	if err := checkIDStage(id, stage); err != nil {
		return Status{}, err
	}
	out, err := c.call(ctx, "status", "--id", id, "--stage", stage)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return s, fmt.Errorf("decode jobrunner status: %w", err)
	}
	if s.JobID != id || s.Stage != stage {
		return s, errors.New("jobrunner status does not match requested job")
	}
	return s, nil
}

func (c Client) Logs(ctx context.Context, id, stage string, tail int) (string, error) {
	if err := checkIDStage(id, stage); err != nil {
		return "", err
	}
	if tail < 0 || tail > 1000 {
		return "", errors.New("tail must be between 0 and 1000")
	}
	return c.call(ctx, "logs", "--id", id, "--stage", stage, "--tail", fmt.Sprint(tail))
}

func (c Client) Stop(ctx context.Context, id, stage string) error {
	if err := checkIDStage(id, stage); err != nil {
		return err
	}
	_, err := c.call(ctx, "stop", "--id", id, "--stage", stage)
	return err
}

// ReadResult should only be called after GetStatus reports an exited container
// with exit code zero. The caller still validates the task-specific result.
func (c Client) ReadResult(ctx context.Context, s Status, maxBytes int64) (json.RawMessage, error) {
	if s.State.Status != "exited" || s.State.ExitCode != 0 {
		return nil, errors.New("stage has not completed successfully")
	}
	if err := checkIDStage(s.JobID, s.Stage); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > 64<<20 {
		return nil, errors.New("maxBytes must be between 1 and 64 MiB")
	}
	b, err := c.call(ctx, "result", "--id", s.JobID, "--stage", s.Stage,
		"--max-bytes", strconv.FormatInt(maxBytes, 10))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxBytes || !json.Valid([]byte(b)) {
		return nil, errors.New("invalid or oversized result.json")
	}
	return json.RawMessage(b), nil
}

func checkIDStage(id, stage string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid job ID")
	}
	if stage != "preview" && stage != "apply" {
		return errors.New("invalid stage")
	}
	return nil
}

func (c Client) call(parent context.Context, command string, args ...string) (string, error) {
	return c.callWithInput(parent, nil, command, args...)
}

func (c Client) callWithInput(parent context.Context, input []byte, command string, args ...string) (string, error) {
	if c.Binary == "" || c.Config == "" {
		return "", errors.New("Binary and Config are required")
	}
	// The remote call is short; the task container continues after SSH exits.
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	all := append([]string{command, "--config", c.Config}, args...)
	var cmd *exec.Cmd
	if c.Remote == "" {
		cmd = exec.CommandContext(ctx, c.Binary, all...)
	} else {
		if strings.HasPrefix(c.Remote, "-") || strings.ContainsAny(c.Remote, " \t\r\n") {
			return "", errors.New("invalid SSH destination")
		}
		port := c.SSHPort
		if port == 0 {
			port = 22
		}
		if port < 1 || port > 65535 {
			return "", errors.New("invalid SSH port")
		}
		sshBinary := c.SSHBinary
		if sshBinary == "" {
			sshBinary = "ssh"
		}
		quoted := make([]string, 0, len(all)+1)
		quoted = append(quoted, shellQuote(c.Binary))
		for _, arg := range all {
			quoted = append(quoted, shellQuote(arg))
		}
		cmd = exec.CommandContext(ctx, sshBinary, "-T", "-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10",
			"-p", strconv.Itoa(port), c.Remote, "exec "+strings.Join(quoted, " "))
	}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("jobrunner %s: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// OpenSSH sends its remote command to a shell; quote every argument separately.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
