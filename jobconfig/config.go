// Package jobconfig defines the task contract shared by jobrunner and Go callers.
package jobconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	WorkRoot     string              `json:"work_root"`
	HostWorkRoot string              `json:"host_work_root,omitempty"`
	Tasks        map[string]TaskType `json:"tasks"`
}

type TaskType struct {
	Image                string            `json:"image"`
	Memory               string            `json:"memory"`
	CPUs                 string            `json:"cpus"`
	Network              string            `json:"network"`
	Mode                 string            `json:"mode,omitempty"`
	Command              []string          `json:"command,omitempty"`
	// SchemaArgument enables machine-readable discovery from the fixed command.
	SchemaArgument       string            `json:"schema_argument,omitempty"`
	WorkDir              string            `json:"work_dir,omitempty"`
	Mounts               []TaskMount       `json:"mounts,omitempty"`
	EnvFile              string            `json:"env_file,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
	EnvironmentVariables []EnvironmentVar  `json:"environment_variables,omitempty"`
	Parameters           []TaskParameter   `json:"parameters,omitempty"`
}

type TaskMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type TaskParameter struct {
	Name          string   `json:"name"`
	Flag          string   `json:"flag"`
	Type          string   `json:"type"`
	Required      bool     `json:"required,omitempty"`
	Pattern       string   `json:"pattern,omitempty"`
	AllowedValues []string `json:"allowed_values,omitempty"`
	PathPrefix    string   `json:"path_prefix,omitempty"`
}

type EnvironmentVar struct {
	Name     string `json:"name"`
	Required bool   `json:"required,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
}

// Schema is the only accepted stdout document for a schema query.
// Version 1 contains declarations, never runtime values or credentials.
type Schema struct {
	Version              int              `json:"version"`
	Parameters           []TaskParameter  `json:"parameters"`
	EnvironmentVariables []EnvironmentVar `json:"environment_variables"`
}

func (t TaskType) WithSchema(schema Schema) (TaskType, error) {
	if schema.Version != 1 {
		return TaskType{}, fmt.Errorf("unsupported job schema version %d", schema.Version)
	}
	if len(schema.Parameters) > 128 || len(schema.EnvironmentVariables) > 128 {
		return TaskType{}, errors.New("job schema has too many declarations")
	}
	t.Parameters = schema.Parameters
	t.EnvironmentVariables = schema.EnvironmentVariables
	parameterNames := make(map[string]bool)
	flags := make(map[string]bool)
	for _, spec := range t.Parameters {
		if !validName.MatchString(spec.Name) || !validFlag.MatchString(spec.Flag) ||
			parameterNames[spec.Name] || flags[spec.Flag] {
			return TaskType{}, fmt.Errorf("invalid or duplicate parameter declaration %q", spec.Name)
		}
		parameterNames[spec.Name], flags[spec.Flag] = true, true
		if spec.Type != "string" && spec.Type != "path" && spec.Type != "integer" &&
			spec.Type != "number" && spec.Type != "boolean" {
			return TaskType{}, fmt.Errorf("invalid parameter type %q", spec.Type)
		}
		if spec.Type == "path" && (!strings.HasSuffix(spec.PathPrefix, "/") ||
			(path.IsAbs(spec.PathPrefix) && spec.PathPrefix != "/job/files/") ||
			path.Clean(spec.PathPrefix) != strings.TrimSuffix(spec.PathPrefix, "/") ||
			strings.Contains("/"+spec.PathPrefix, "/../")) {
			return TaskType{}, fmt.Errorf("invalid path prefix for %q", spec.Name)
		}
		if spec.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + spec.Pattern + ")$"); err != nil {
				return TaskType{}, fmt.Errorf("invalid pattern for %q: %w", spec.Name, err)
			}
		}
	}
	envNames := make(map[string]bool)
	for _, spec := range t.EnvironmentVariables {
		if !validEnvName.MatchString(spec.Name) || envNames[spec.Name] {
			return TaskType{}, fmt.Errorf("invalid or duplicate environment variable %q", spec.Name)
		}
		if _, fixed := t.Environment[spec.Name]; fixed {
			return TaskType{}, fmt.Errorf("environment variable %q is also fixed", spec.Name)
		}
		envNames[spec.Name] = true
	}
	return t, nil
}

// StartRequest carries runtime values. Environment values never enter argv.
type StartRequest struct {
	Input       json.RawMessage            `json:"input"`
	Parameters  map[string]json.RawMessage `json:"parameters,omitempty"`
	Environment map[string]string          `json:"environment,omitempty"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var validFlag = regexp.MustCompile(`^-{1,2}[A-Za-z][A-Za-z0-9-]*$`)
var validEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var integerValue = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func Load(filename string) (Config, error) {
	v := viper.New()
	v.SetConfigFile(filename)
	v.SetEnvPrefix("JOBRUNNER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "__"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		return Config{}, err
	}
	// Decode the original JSON to retain case-sensitive Docker environment names.
	b, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	// Only fixed scalar settings can be overridden. Runtime credentials are
	// supplied separately by StartRequest.Environment.
	override := func(key string, value *string) {
		if v.IsSet(key) {
			*value = v.GetString(key)
		}
	}
	override("work_root", &c.WorkRoot)
	override("host_work_root", &c.HostWorkRoot)
	for name, task := range c.Tasks {
		prefix := "tasks." + name + "."
		override(prefix+"image", &task.Image)
		override(prefix+"memory", &task.Memory)
		override(prefix+"cpus", &task.CPUs)
		override(prefix+"network", &task.Network)
		override(prefix+"mode", &task.Mode)
		override(prefix+"schema_argument", &task.SchemaArgument)
		override(prefix+"work_dir", &task.WorkDir)
		override(prefix+"env_file", &task.EnvFile)
		for i := range task.Mounts {
			var err error
			task.Mounts[i].Source, err = expandConfigEnv(task.Mounts[i].Source)
			if err != nil {
				return Config{}, fmt.Errorf("task %q mount %d: %w", name, i, err)
			}
		}
		c.Tasks[name] = task
	}
	return c, nil
}

// expandConfigEnv resolves host paths in mount sources. An unset variable is
// an error so a misspelled path never silently becomes a different mount.
func expandConfigEnv(value string) (string, error) {
	var missing string
	result := os.Expand(value, func(name string) string {
		if !validEnvName.MatchString(name) {
			missing = name
			return ""
		}
		found, ok := os.LookupEnv(name)
		if !ok {
			missing = name
		}
		return found
	})
	if missing != "" {
		return "", fmt.Errorf("missing environment variable %q", missing)
	}
	return result, nil
}

func (c Config) Task(name string) (TaskType, error) {
	t, ok := c.Tasks[name]
	if !ok {
		return TaskType{}, fmt.Errorf("unknown task type %q", name)
	}
	return t, nil
}

func (t TaskType) FormatParameters(values map[string]json.RawMessage) ([]string, error) {
	if len(values) > 128 {
		return nil, errors.New("too many parameters")
	}
	seen := make(map[string]bool)
	args := make([]string, 0, len(values))
	for _, spec := range t.Parameters {
		if !validName.MatchString(spec.Name) || !validFlag.MatchString(spec.Flag) || seen[spec.Name] {
			return nil, fmt.Errorf("invalid or duplicate parameter configuration %q", spec.Name)
		}
		seen[spec.Name] = true
		typ := spec.Type
		if typ == "" {
			typ = "string" // Compatibility with earlier configurations.
		}
		if typ != "string" && typ != "path" && typ != "integer" && typ != "number" && typ != "boolean" {
			return nil, fmt.Errorf("invalid type for parameter %q", spec.Name)
		}
		value, present := values[spec.Name]
		if !present {
			if spec.Required {
				return nil, fmt.Errorf("missing required parameter %q", spec.Name)
			}
			continue
		}
		if len(value) == 0 || len(value) > 4096 {
			return nil, fmt.Errorf("invalid value for parameter %q", spec.Name)
		}
		var formatted string
		switch typ {
		case "string", "path":
			if err := json.Unmarshal(value, &formatted); err != nil {
				return nil, fmt.Errorf("parameter %q must be a string", spec.Name)
			}
		case "integer":
			if !integerValue.Match(value) {
				return nil, fmt.Errorf("parameter %q must be an integer", spec.Name)
			}
			if _, err := strconv.ParseInt(string(value), 10, 64); err != nil {
				return nil, fmt.Errorf("parameter %q is outside int64 range", spec.Name)
			}
			formatted = string(value)
		case "number":
			if value[0] == '"' || !json.Valid(value) {
				return nil, fmt.Errorf("parameter %q must be a number", spec.Name)
			}
			if _, err := strconv.ParseFloat(string(value), 64); err != nil {
				return nil, fmt.Errorf("parameter %q must be a finite number", spec.Name)
			}
			formatted = string(value)
		case "boolean":
			if string(value) != "true" && string(value) != "false" {
				return nil, fmt.Errorf("parameter %q must be a boolean", spec.Name)
			}
			formatted = string(value)
		}
		if len(formatted) == 0 || len(formatted) > 512 || strings.ContainsAny(formatted, "\x00\r\n") {
			return nil, fmt.Errorf("invalid value for parameter %q", spec.Name)
		}
		if typ == "path" {
			if spec.PathPrefix == "" || path.Clean(formatted) != formatted || !strings.HasPrefix(formatted, spec.PathPrefix) || strings.Contains("/"+formatted+"/", "/../") {
				return nil, fmt.Errorf("invalid path for parameter %q", spec.Name)
			}
		}
		if spec.Pattern != "" {
			re, err := regexp.Compile("^(?:" + spec.Pattern + ")$")
			if err != nil {
				return nil, fmt.Errorf("invalid pattern for %q: %w", spec.Name, err)
			}
			if !re.MatchString(formatted) {
				return nil, fmt.Errorf("value for %q does not match its pattern", spec.Name)
			}
		}
		if len(spec.AllowedValues) != 0 {
			allowed := false
			for _, option := range spec.AllowedValues {
				if formatted == option {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("value for %q is not allowed", spec.Name)
			}
		}
		args = append(args, spec.Flag+"="+formatted)
	}
	for name := range values {
		if !seen[name] {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	return args, nil
}

func (t TaskType) ValidateEnvironment(values map[string]string) error {
	seen := make(map[string]bool)
	for _, spec := range t.EnvironmentVariables {
		if !validEnvName.MatchString(spec.Name) || seen[spec.Name] {
			return fmt.Errorf("invalid or duplicate environment variable %q", spec.Name)
		}
		seen[spec.Name] = true
		if _, fixed := t.Environment[spec.Name]; fixed {
			return fmt.Errorf("environment variable %q is also fixed", spec.Name)
		}
		if value, present := values[spec.Name]; spec.Required && (!present || value == "") {
			return fmt.Errorf("missing required environment variable %q", spec.Name)
		}
	}
	for name, value := range values {
		if !seen[name] || strings.ContainsAny(value, "\x00\r\n") || len(value) > 8192 {
			return fmt.Errorf("unknown or invalid environment variable %q", name)
		}
	}
	return nil
}
