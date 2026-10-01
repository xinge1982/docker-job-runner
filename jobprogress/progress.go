// Package jobprogress defines the worker's line-oriented progress protocol.
package jobprogress

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"
)

const Prefix = "JOBRUNNER_PROGRESS "
const MaxLineBytes = 64 << 10

// Progress reports overall percentage and optional counts for the current phase.
// Nil Percent means the amount of remaining work is unknown.
type Progress struct {
	Version   int      `json:"version"`
	Percent   *float64 `json:"percent,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Completed *int64   `json:"completed,omitempty"`
	Total     *int64   `json:"total,omitempty"`
	Message   string   `json:"message,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

// ParseLine accepts only timestamped Docker log lines with the exact prefix.
// The timestamp comes from Docker, never from the worker's JSON.
func ParseLine(line string) (*Progress, bool) {
	if len(line) > MaxLineBytes {
		return nil, false
	}
	stamp, payload, ok := strings.Cut(line, " ")
	if !ok || !strings.HasPrefix(payload, Prefix) {
		return nil, false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return nil, false
	}
	var p Progress
	if err := json.Unmarshal([]byte(strings.TrimPrefix(payload, Prefix)), &p); err != nil {
		return nil, false
	}
	if p.Version != 1 || (p.Percent == nil && p.Phase == "" && p.Message == "") {
		return nil, false
	}
	if p.Percent != nil && (math.IsNaN(*p.Percent) || math.IsInf(*p.Percent, 0) || *p.Percent < 0 || *p.Percent > 100) {
		return nil, false
	}
	if p.Completed != nil && *p.Completed < 0 || p.Total != nil && *p.Total < 0 {
		return nil, false
	}
	if p.Completed != nil && p.Total != nil && *p.Completed > *p.Total {
		return nil, false
	}
	p.UpdatedAt = timestamp.UTC().Format(time.RFC3339Nano)
	return &p, true
}

// Collector consumes combined stdout/stderr without retaining ordinary logs.
// Overlong lines are discarded until their newline; memory use stays bounded.
// Docker may deliver the two streams out of timestamp order.
type Collector struct {
	mu      sync.Mutex
	line    []byte
	discard bool
	latest  *Progress
}

func (c *Collector) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ch := range b {
		if ch == '\n' {
			c.finishLine()
		} else if !c.discard {
			if len(c.line) == MaxLineBytes {
				c.line = c.line[:0]
				c.discard = true
			} else {
				c.line = append(c.line, ch)
			}
		}
	}
	return len(b), nil
}

func (c *Collector) finishLine() {
	if !c.discard {
		if p, ok := ParseLine(strings.TrimSuffix(string(c.line), "\r")); ok {
			timestamp, _ := time.Parse(time.RFC3339Nano, p.UpdatedAt)
			var previous time.Time
			if c.latest != nil {
				previous, _ = time.Parse(time.RFC3339Nano, c.latest.UpdatedAt)
			}
			if !timestamp.Before(previous) {
				c.latest = p
			}
		}
	}
	c.line = c.line[:0]
	c.discard = false
}

// Latest finishes an unterminated final line. Call after the log command exits.
func (c *Collector) Latest() *Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.line) != 0 || c.discard {
		c.finishLine()
	}
	return c.latest
}

// ForState normalizes successful completion. Failed stages retain their last
// percentage; a worker reporting 100 does not change the container's state.
func ForState(p *Progress, status string, running bool, exitCode int, finishedAt string) *Progress {
	if status != "exited" || running || exitCode != 0 {
		return p
	}
	result := Progress{Version: 1}
	if p != nil {
		result = *p
	}
	percent := 100.0
	result.Percent = &percent
	if result.Phase == "" {
		result.Phase = "done"
	}
	if t, err := time.Parse(time.RFC3339Nano, finishedAt); err == nil && t.Year() > 1 {
		result.UpdatedAt = t.UTC().Format(time.RFC3339Nano)
	}
	return &result
}
