package artex

// Explicit provider configuration file loader.
//
// There is deliberately no environment variable, no default host and no default
// token. A deployment that wants to talk to ARTEX names one JSON file and the
// loader refuses everything that could silently widen access:
//
//   - the file must be a regular file (not a symlink, device or directory),
//   - it must not be readable or writable by group/other (mode 0600 or stricter),
//   - "base_url" and "token" must both be present,
//   - "allow_writes" is false unless it is literally true in the file,
//   - every bound is explicit and checked instead of clamped silently.
//
// File shape:
//
//	{
//	  "base_url": "http://127.0.0.1:8787",
//	  "token": "<ARTEX JWT>",
//	  "allow_writes": false,
//	  "timeout_seconds": 10,
//	  "max_attempts": 3,
//	  "retry_base_millis": 100
//	}
//
// The file is a secret container because of "token"; the error text never
// includes the token or the file content.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ProviderFile is the on-disk shape. It mirrors Config but with JSON tags and
// second/millisecond units so an operator file does not carry Go durations.
type ProviderFile struct {
	BaseURL         string `json:"base_url"`
	Token           string `json:"token"`
	AllowWrites     bool   `json:"allow_writes"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
	MaxAttempts     int    `json:"max_attempts"`
	RetryBaseMillis int    `json:"retry_base_millis"`
}

// LoadConfigFile reads one provider file and returns the Config it describes.
// Any violation is an error: an invalid provider file must stop startup, not
// degrade into "ARTEX is unreachable" or a read-only surprise.
func LoadConfigFile(path string) (Config, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return Config{}, errors.New("artex provider file path is required")
	}
	info, err := os.Lstat(trimmed)
	if err != nil {
		return Config{}, fmt.Errorf("artex provider file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Config{}, fmt.Errorf("artex provider file %q must not be a symlink", trimmed)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("artex provider file %q must be a regular file", trimmed)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Config{}, fmt.Errorf("artex provider file %q must not be accessible by group/other (mode %#o, want 0600 or stricter)", trimmed, perm)
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return Config{}, fmt.Errorf("artex provider file: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Config{}, fmt.Errorf("artex provider file %q is empty", trimmed)
	}
	var file ProviderFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return Config{}, fmt.Errorf("artex provider file %q: %w", trimmed, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("artex provider file %q has trailing content", trimmed)
	}
	cfg := Config{
		BaseURL:     strings.TrimSpace(file.BaseURL),
		Token:       strings.TrimSpace(file.Token),
		AllowWrites: file.AllowWrites,
	}
	if cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("artex provider file %q: base_url is required", trimmed)
	}
	if cfg.Token == "" {
		return Config{}, fmt.Errorf("artex provider file %q: token is required", trimmed)
	}
	if file.TimeoutSeconds < 0 || file.TimeoutSeconds > 60 {
		return Config{}, fmt.Errorf("artex provider file %q: timeout_seconds must be 0..60", trimmed)
	}
	if file.MaxAttempts < 0 || file.MaxAttempts > maxAttempts {
		return Config{}, fmt.Errorf("artex provider file %q: max_attempts must be 0..%d", trimmed, maxAttempts)
	}
	if file.RetryBaseMillis < 0 || file.RetryBaseMillis > 2000 {
		return Config{}, fmt.Errorf("artex provider file %q: retry_base_millis must be 0..2000", trimmed)
	}
	if file.TimeoutSeconds > 0 {
		cfg.Timeout = time.Duration(file.TimeoutSeconds) * time.Second
	}
	cfg.Retry = RetryPolicy{MaxAttempts: file.MaxAttempts, BaseDelay: time.Duration(file.RetryBaseMillis) * time.Millisecond}
	return cfg, nil
}
