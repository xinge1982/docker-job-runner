package jobconfig

import (
	"encoding/json"
	"reflect"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadViperEnvironment(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	contents := `{"work_root":"/old/jobs","tasks":{"road_job":{"image":"alpine:3.14","network":"old","command":["/app/road"],"mounts":[{"source":"${PROGRAM_PATH}/road","target":"/app/road","read_only":true}],"environment":{"POSTGRES_HOST":"postgres"}}}}`
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOBRUNNER_WORK_ROOT", "/new/jobs")
	t.Setenv("JOBRUNNER_HOST_WORK_ROOT", "/host/jobs")
	t.Setenv("JOBRUNNER_TASKS__ROAD_JOB__NETWORK", "sign_default")
	t.Setenv("PROGRAM_PATH", "/srv/programs")
	config, err := Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if config.WorkRoot != "/new/jobs" || config.HostWorkRoot != "/host/jobs" {
		t.Fatalf("unexpected roots: %#v", config)
	}
	task := config.Tasks["road_job"]
	if task.Network != "sign_default" || task.Image != "alpine:3.14" || task.Mounts[0].Source != "/srv/programs/road" {
		t.Fatalf("unexpected task: %#v", task)
	}
	if _, ok := task.Environment["POSTGRES_HOST"]; !ok {
		t.Fatalf("environment name lost its case: %#v", task.Environment)
	}
}

func TestLoadMissingMountEnvironment(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(filename, []byte(`{"tasks":{"job":{"mounts":[{"source":"${MISSING_MOUNT_PATH}/data","target":"/data"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filename); err == nil {
		t.Fatal("expected an error for an unset mount path variable")
	}
}

func TestFormatParameters(t *testing.T) {
	task := TaskType{Parameters: []TaskParameter{
		{Name: "config", Flag: "-c", Type: "path", Required: true, PathPrefix: "networks/"},
		{Name: "count", Flag: "--count", Type: "integer"},
		{Name: "enabled", Flag: "--enabled", Type: "boolean"},
		{Name: "kind", Flag: "--kind", Type: "string", AllowedValues: []string{"bridges"}},
	}}
	values := map[string]json.RawMessage{
		"config": json.RawMessage(`"networks/config.yaml"`),
		"count": json.RawMessage(`4`),
		"enabled": json.RawMessage(`true`),
		"kind": json.RawMessage(`"bridges"`),
	}
	got, err := task.FormatParameters(values)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c=networks/config.yaml", "--count=4", "--enabled=true", "--kind=bridges"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
	for _, bad := range []map[string]json.RawMessage{
		{"config": json.RawMessage(`"networks/../secret"`)},
		{"config": json.RawMessage(`"networks/config.yaml"`), "count": json.RawMessage(`"4"`)},
		{"config": json.RawMessage(`"networks/config.yaml"`), "kind": json.RawMessage(`"unknown"`)},
		{"config": json.RawMessage(`"networks/config.yaml"`), "arbitrary": json.RawMessage(`"value"`)},
	} {
		if _, err := task.FormatParameters(bad); err == nil {
			t.Fatalf("accepted invalid parameters: %v", bad)
		}
	}
}

func TestUploadedFilePathParameter(t *testing.T) {
	task, err := (TaskType{}).WithSchema(Schema{Version: 1, Parameters: []TaskParameter{{
		Name: "geojson", Flag: "--geojson", Type: "path", Required: true, PathPrefix: "/job/files/",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := task.FormatParameters(map[string]json.RawMessage{"geojson": json.RawMessage(`"/job/files/roads.geojson"`)})
	if err != nil || !reflect.DeepEqual(got, []string{"--geojson=/job/files/roads.geojson"}) {
		t.Fatalf("format uploaded file path: %v, %v", got, err)
	}
	for _, invalid := range []string{`"/job/files/../secret"`, `"/etc/passwd"`} {
		if _, err := task.FormatParameters(map[string]json.RawMessage{"geojson": json.RawMessage(invalid)}); err == nil {
			t.Fatalf("accepted invalid upload path %s", invalid)
		}
	}
}

func TestValidateEnvironment(t *testing.T) {
	task := TaskType{EnvironmentVariables: []EnvironmentVar{
		{Name: "POSTGRES_HOST", Required: true},
		{Name: "POSTGRES_PASSWORD", Required: true, Secret: true},
	}}
	if err := task.ValidateEnvironment(map[string]string{"POSTGRES_HOST": "db", "POSTGRES_PASSWORD": "secret"}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]string{
		{"POSTGRES_HOST": "db"},
		{"POSTGRES_HOST": "db", "POSTGRES_PASSWORD": ""},
		{"POSTGRES_HOST": "db", "POSTGRES_PASSWORD": "secret", "UNLISTED": "value"},
	} {
		if err := task.ValidateEnvironment(invalid); err == nil {
			t.Fatalf("accepted invalid environment: %v", invalid)
		}
	}
}

func TestWithSchemaRejectsInvalidDeclarations(t *testing.T) {
	base := TaskType{Environment: map[string]string{"TZ": "Asia/Shanghai"}}
	valid := Schema{Version: 1, Parameters: []TaskParameter{
		{Name: "config", Flag: "-c", Type: "path", PathPrefix: "networks/"},
	}, EnvironmentVariables: []EnvironmentVar{{Name: "POSTGRES_HOST", Required: true}}}
	if _, err := base.WithSchema(valid); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Parameters = append([]TaskParameter(nil), valid.Parameters...)
	bad.Parameters = append(bad.Parameters, valid.Parameters[0])
	if _, err := base.WithSchema(bad); err == nil {
		t.Fatal("duplicate parameter accepted")
	}
	bad = valid
	bad.Version = 2
	if _, err := base.WithSchema(bad); err == nil {
		t.Fatal("unsupported schema version accepted")
	}
}
