package jobprogress

import (
	"strings"
	"testing"
)

const stamp = "2026-10-01T12:00:00.000000001Z "

func TestParseLine(t *testing.T) {
	valid := stamp + Prefix + `{"version":1,"percent":0,"completed":0,"total":10,"updated_at":"forged"}`
	p, ok := ParseLine(valid)
	if !ok || p.Percent == nil || *p.Percent != 0 || p.UpdatedAt != strings.TrimSpace(stamp) {
		t.Fatalf("invalid result: %+v", p)
	}
	for _, body := range []string{
		`{"version":2,"percent":50}`, `{"version":1,"percent":101}`,
		`{"version":1,"percent":-1}`, `{"version":1,"percent":"50"}`,
		`{"version":1}`, `{"version":1,"completed":-1,"phase":"work"}`,
		`{"version":1,"completed":3,"total":2,"phase":"work"}`, `{"version":1,"percent":50} trailing`,
	} {
		if _, ok := ParseLine(stamp + Prefix + body); ok {
			t.Errorf("accepted %s", body)
		}
	}
	if _, ok := ParseLine("ordinary log " + Prefix + `{"version":1,"percent":50}`); ok {
		t.Fatal("accepted ordinary log")
	}
	if p, ok := ParseLine(stamp + Prefix + `{"version":1,"phase":"prepare"}`); !ok || p.Percent != nil {
		t.Fatal("unknown percentage must remain nil")
	}
}

func TestCollectorChunkedAndOutOfOrder(t *testing.T) {
	input := stamp + Prefix + `{"version":1,"percent":60}` + "\n" +
		"2026-10-01T11:59:59Z " + Prefix + `{"version":1,"percent":40}` + "\n" +
		strings.Repeat("x", MaxLineBytes+100) + "\n" + stamp + Prefix + "invalid\n" +
		strings.Repeat("ordinary log\n", 20000)
	var c Collector
	for i := 0; i < len(input); i += 7 {
		end := i + 7
		if end > len(input) {
			end = len(input)
		}
		if n, err := c.Write([]byte(input[i:end])); err != nil || n != end-i {
			t.Fatal(n, err)
		}
	}
	p := c.Latest()
	if p == nil || p.Percent == nil || *p.Percent != 60 {
		t.Fatalf("lost progress: %+v", p)
	}
	c.Write([]byte("2026-10-01T12:01:00Z " + Prefix + `{"version":1,"percent":80}`))
	if *c.Latest().Percent != 80 {
		t.Fatal("unterminated final line lost")
	}
}

func TestCompletion(t *testing.T) {
	value := 42.0
	p := &Progress{Version: 1, Percent: &value, Phase: "save"}
	for _, status := range []string{"running", "created", "dead"} {
		if ForState(p, status, false, 0, "") != p {
			t.Fatal("normalized non-success")
		}
	}
	if ForState(p, "exited", false, 1, "") != p {
		t.Fatal("normalized failure")
	}
	if ForState(p, "exited", true, 0, "") != p {
		t.Fatal("normalized running container")
	}
	done := ForState(p, "exited", false, 0, "2026-10-01T12:02:00Z")
	if *done.Percent != 100 || *p.Percent != 42 || done.UpdatedAt != "2026-10-01T12:02:00Z" {
		t.Fatal("completion mutated original or lost time")
	}
	if done := ForState(nil, "exited", false, 0, ""); done == nil || *done.Percent != 100 {
		t.Fatal("legacy success missing")
	}
}
