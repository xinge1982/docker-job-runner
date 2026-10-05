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
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xinge1982/docker-job-runner/jobconfig"
	"github.com/xinge1982/docker-job-runner/jobprogress"
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

// Progress is the shared worker progress contract. Percent may be nil.
type Progress = jobprogress.Progress

type Status struct {
	Progress      *Progress      `json:"progress,omitempty"`
	ProgressError string         `json:"progress_error,omitempty"`
	JobID         string         `json:"job_id"`
	Stage         string         `json:"stage"`
	ContainerID   string         `json:"container_id"`
	ContainerName string         `json:"container_name"`
	State         ContainerState `json:"container_state"`
	Output        string         `json:"output"`
}

type StartResult = jobconfig.StartResult

// CompletedJob reports retained stage containers for an instance whose
// existing stages have all finished. A nonzero exit code is still completed.
type CompletedJob struct {
	JobID  string                    `json:"job_id"`
	Stages map[string]ContainerState `json:"stages"`
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
	return c.start(ctx, id, taskType, "preview", params, nil)
}

func (c Client) StartApply(ctx context.Context, id, taskType string, approved json.RawMessage) (string, error) {
	return c.start(ctx, id, taskType, "apply", approved, nil)
}

// Command parameters are validated against the task type's configured allowlist.
// They are separate from the JSON document mounted at /job/input.json.
func (c Client) StartPreviewWithParams(ctx context.Context, id, taskType string, input json.RawMessage, params map[string]string) (string, error) {
	return c.start(ctx, id, taskType, "preview", input, params)
}

func (c Client) StartApplyWithParams(ctx context.Context, id, taskType string, input json.RawMessage, params map[string]string) (string, error) {
	return c.start(ctx, id, taskType, "apply", input, params)
}

// StartRunWithParams executes a direct task without a preview/apply workflow.
func (c Client) StartRunWithParams(ctx context.Context, id, taskType string, input json.RawMessage, params map[string]string) (string, error) {
	return c.start(ctx, id, taskType, "run", input, params)
}

// DescribeTask exposes the configured parameter and environment requirements.
// It works for both local and SSH-backed runners.
func (c Client) DescribeTask(ctx context.Context, taskType string) (jobconfig.TaskType, error) {
	if taskType == "" {
		return jobconfig.TaskType{}, errors.New("task type is required")
	}
	out, err := c.call(ctx, "describe", "--type", taskType)
	if err != nil {
		return jobconfig.TaskType{}, err
	}
	var task jobconfig.TaskType
	if err := json.Unmarshal([]byte(out), &task); err != nil {
		return task, fmt.Errorf("decode task specification: %w", err)
	}
	return task, nil
}

// ListTaskTypes returns the sorted task names configured in the runner.
// It intentionally exposes no image, mount, command, or environment details.
func (c Client) ListTaskTypes(ctx context.Context) ([]string, error) {
	out, err := c.call(ctx, "list-types")
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal([]byte(out), &names); err != nil {
		return nil, fmt.Errorf("decode task type list: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	for index, name := range names {
		if name == "" || (index > 0 && names[index-1] >= name) {
			return nil, errors.New("jobrunner task type list is invalid or unsorted")
		}
	}
	return names, nil
}

func (c Client) StartRunRequest(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (string, error) {
	return c.startRequest(ctx, id, taskType, "run", request)
}

func (c Client) StartPreviewRequest(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (string, error) {
	return c.startRequest(ctx, id, taskType, "preview", request)
}

func (c Client) StartApplyRequest(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (string, error) {
	return c.startRequest(ctx, id, taskType, "apply", request)
}

func (c Client) StartRunRequestDetailed(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (StartResult, error) {
	return c.startRequestDetailed(ctx, id, taskType, "run", request)
}

func (c Client) StartPreviewRequestDetailed(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (StartResult, error) {
	return c.startRequestDetailed(ctx, id, taskType, "preview", request)
}

func (c Client) StartApplyRequestDetailed(ctx context.Context, id, taskType string, request jobconfig.StartRequest) (StartResult, error) {
	return c.startRequestDetailed(ctx, id, taskType, "apply", request)
}

func (c Client) startRequest(ctx context.Context, id, taskType, stage string, request jobconfig.StartRequest) (string, error) {
	b, err := encodeStartRequest(id, stage, request)
	if err != nil {
		return "", err
	}
	out, err := c.callWithInput(ctx, b, "start", "--id", id, "--type", taskType,
		"--stage", stage, "--request", "-")
	if err != nil {
		return "", err
	}
	return parseContainerID(out)
}

func (c Client) startRequestDetailed(ctx context.Context, id, taskType, stage string, request jobconfig.StartRequest) (StartResult, error) {
	b, err := encodeStartRequest(id, stage, request)
	if err != nil {
		return StartResult{}, err
	}
	out, err := c.callWithInput(ctx, b, "start", "--id", id, "--type", taskType,
		"--stage", stage, "--request", "-", "--response-format", "json")
	if err != nil {
		return StartResult{}, err
	}
	var result StartResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return StartResult{}, fmt.Errorf("decode start response: %w", err)
	}
	if result.JobID != id || result.Stage != stage || result.ContainerID == "" || result.ContainerName != "job-"+id+"-"+stage {
		return StartResult{}, errors.New("start response does not match request")
	}
	return result, nil
}

func encodeStartRequest(id, stage string, request jobconfig.StartRequest) ([]byte, error) {
	if err := checkIDStage(id, stage); err != nil {
		return nil, err
	}
	if !json.Valid(request.Input) || len(request.Input) > 32<<20 {
		return nil, errors.New("input must be valid JSON and at most 32 MiB")
	}
	b, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(b) > 32<<20 {
		return nil, errors.New("start request exceeds 32 MiB")
	}
	return b, nil
}

func (c Client) start(ctx context.Context, id, taskType, stage string, input json.RawMessage, params map[string]string) (string, error) {
	if !validID.MatchString(id) {
		return "", errors.New("invalid job ID")
	}
	if !json.Valid(input) {
		return "", errors.New("input must be valid JSON")
	}
	if len(input) > 32<<20 {
		return "", errors.New("input exceeds 32 MiB")
	}
	if params == nil {
		params = map[string]string{}
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	out, err := c.callWithInput(ctx, input, "start", "--id", id, "--type", taskType,
		"--stage", stage, "--input", "-", "--params", string(encoded))
	if err != nil {
		return "", err
	}
	return parseContainerID(out)
}

func parseContainerID(out string) (string, error) {
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

// UploadFile streams a known-size file to the job's immutable files directory.
// Use the returned Path in the job's input or configured command parameters.
// A caller can provide a SHA-256 digest to verify the transfer end to end.
func (c Client) UploadFile(ctx context.Context, id, name string, size int64, sha256Hex string, source io.Reader) (UploadResult, error) {
	if !validID.MatchString(id) || source == nil {
		return UploadResult{}, errors.New("invalid job ID or upload source")
	}
	if size < 0 || size > 8<<30 {
		return UploadResult{}, errors.New("upload size is outside the allowed range")
	}
	cmd, err := c.newCommand(ctx, "upload", "--id", id, "--file-name", name,
		"--size", strconv.FormatInt(size, 10), "--sha256", sha256Hex)
	if err != nil {
		return UploadResult{}, err
	}
	cmd.Stdin = source
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return UploadResult{}, fmt.Errorf("jobrunner upload: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var result UploadResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return result, fmt.Errorf("decode upload response: %w", err)
	}
	if result.Name != name || result.Size != size || result.Path != "/job/files/"+name {
		return result, errors.New("upload response does not match request")
	}
	return result, nil
}

type UploadResult struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

// DownloadArchive streams a tar.gz snapshot of the job directory into dst.
// If it fails, dst may already contain a partial archive; discard that output.
func (c Client) DownloadArchive(ctx context.Context, id string, dst io.Writer) error {
	if !validID.MatchString(id) || dst == nil {
		return errors.New("invalid job ID or archive destination")
	}
	cmd, err := c.newCommand(ctx, "archive", "--id", id)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = dst, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jobrunner archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ListCompleted returns job IDs with a directory and at least one retained
// exited container. Preview-only jobs can appear before an apply stage starts.
func (c Client) ListCompleted(ctx context.Context) ([]CompletedJob, error) {
	out, err := c.call(ctx, "list-completed")
	if err != nil {
		return nil, err
	}
	var jobs []CompletedJob
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		return nil, fmt.Errorf("decode completed jobs: %w", err)
	}
	return jobs, nil
}

// DeleteJob removes all retained stage containers and the instance directory.
// The runner refuses deletion when any stage is still running or pending.
func (c Client) DeleteJob(ctx context.Context, id string) error {
	if !validID.MatchString(id) {
		return errors.New("invalid job ID")
	}
	cmd, err := c.newCommand(ctx, "delete", "--id", id)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jobrunner delete: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
	if stage != "preview" && stage != "apply" && stage != "run" {
		return errors.New("invalid stage")
	}
	return nil
}

func (c Client) call(parent context.Context, command string, args ...string) (string, error) {
	return c.callWithInput(parent, nil, command, args...)
}

func (c Client) callWithInput(parent context.Context, input []byte, command string, args ...string) (string, error) {
	// The remote call is short; the task container continues after SSH exits.
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cmd, err := c.newCommand(ctx, command, args...)
	if err != nil {
		return "", err
	}
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("jobrunner %s: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (c Client) newCommand(ctx context.Context, command string, args ...string) (*exec.Cmd, error) {
	if c.Binary == "" || c.Config == "" {
		return nil, errors.New("Binary and Config are required")
	}
	all := append([]string{command, "--config", c.Config}, args...)
	var cmd *exec.Cmd
	if c.Remote == "" {
		cmd = exec.CommandContext(ctx, c.Binary, all...)
	} else {
		if strings.HasPrefix(c.Remote, "-") || strings.ContainsAny(c.Remote, " \t\r\n") {
			return nil, errors.New("invalid SSH destination")
		}
		port := c.SSHPort
		if port == 0 {
			port = 22
		}
		if port < 1 || port > 65535 {
			return nil, errors.New("invalid SSH port")
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
	return cmd, nil
}

// OpenSSH sends its remote command to a shell; quote every argument separately.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
