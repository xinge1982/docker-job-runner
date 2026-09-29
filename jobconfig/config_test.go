package jobconfig

import (
	"encoding/json"
	"reflect"
	"testing"
)

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
