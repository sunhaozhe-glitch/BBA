package main

// Bounded, secret-free execution evidence. The run directory contains a
// summary.json and steps.jsonl; credentials, message text and request bodies
// are never written.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type callRecord struct {
	Sequence     int    `json:"sequence"`
	Step         string `json:"step"`
	Kind         string `json:"kind"`
	Method       string `json:"method"`
	Target       string `json:"target"`
	CountsBudget bool   `json:"counts_budget"`
	Write        bool   `json:"write"`
	Status       int    `json:"status,omitempty"`
	UTCStart     string `json:"utc_start"`
	UTCEnd       string `json:"utc_end"`
	Error        string `json:"error,omitempty"`
}

type stepRecord struct {
	UTC   string         `json:"utc"`
	Step  string         `json:"step"`
	Detail map[string]any `json:"detail,omitempty"`
}

type evidence struct {
	dir  string
	mu   sync.Mutex
	seq  int
	steps []stepRecord
	calls []callRecord
}

func newEvidence(parent, mode string) (*evidence, error) {
	if parent == "" {
		return nil, fmt.Errorf("evidence directory is required")
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	dir := filepath.Join(parent, "p020-"+mode+"-"+stamp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create evidence directory: %w", err)
	}
	return &evidence{dir: dir}, nil
}

func (e *evidence) step(name string, detail map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if detail == nil {
		detail = map[string]any{}
	}
	e.steps = append(e.steps, stepRecord{UTC: time.Now().UTC().Format(time.RFC3339Nano), Step: name, Detail: detail})
}

func (e *evidence) call(rec callRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	rec.Sequence = e.seq
	e.calls = append(e.calls, rec)
}

func (e *evidence) finish(summary map[string]any) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	summary["utc_finished"] = time.Now().UTC().Format(time.RFC3339Nano)
	summary["evidence_dir"] = e.dir
	if err := writeJSONFile(filepath.Join(e.dir, "summary.json"), summary); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(e.dir, "steps.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, step := range e.steps {
		if err := encoder.Encode(step); err != nil {
			return err
		}
	}
	for _, call := range e.calls {
		if err := encoder.Encode(call); err != nil {
			return err
		}
	}
	return file.Sync()
}

func writeJSONFile(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func randomCode(prefix string) string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(data[:])
}
