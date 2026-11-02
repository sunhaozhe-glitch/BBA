package artex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProviderFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artex.json")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigFileExplicitValues(t *testing.T) {
	path := writeProviderFile(t, `{
		"base_url":"http://127.0.0.1:8787",
		"token":"secret-token",
		"allow_writes":true,
		"timeout_seconds":5,
		"max_attempts":2,
		"retry_base_millis":20
	}`, 0o600)
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.BaseURL != "http://127.0.0.1:8787" || cfg.Token != "secret-token" || !cfg.AllowWrites {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Timeout.String() != "5s" || cfg.Retry.MaxAttempts != 2 || cfg.Retry.BaseDelay.String() != "20ms" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfigFileDefaultsWritesOff(t *testing.T) {
	path := writeProviderFile(t, `{"base_url":"http://127.0.0.1:8787","token":"t"}`, 0o600)
	cfg, err := LoadConfigFile(path)
	if err != nil {
		t.Fatalf("LoadConfigFile: %v", err)
	}
	if cfg.AllowWrites {
		t.Fatal("writes became enabled by omission")
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if client.WritesAllowed() {
		t.Fatal("client allows writes without allow_writes")
	}
}

func TestLoadConfigFileRefusesUnsafeFiles(t *testing.T) {
	valid := `{"base_url":"http://127.0.0.1:8787","token":"secret-token"}`
	groupReadable := writeProviderFile(t, valid, 0o644)
	if _, err := LoadConfigFile(groupReadable); err == nil || !strings.Contains(err.Error(), "group/other") {
		t.Fatalf("0644 file err = %v", err)
	}
	dir := t.TempDir()
	symlink := filepath.Join(dir, "link.json")
	target := writeProviderFile(t, valid, 0o600)
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigFile(symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink err = %v", err)
	}
	if _, err := LoadConfigFile(""); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := LoadConfigFile(filepath.Join(dir, "absent.json")); err == nil {
		t.Fatal("absent file accepted")
	}
	if _, err := LoadConfigFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestLoadConfigFileRefusesMalformedContent(t *testing.T) {
	cases := map[string]string{
		"empty":          "   \n",
		"unknown field":  `{"base_url":"http://127.0.0.1:8787","token":"t","extra":1}`,
		"trailing":       `{"base_url":"http://127.0.0.1:8787","token":"t"} {}`,
		"missing base":   `{"token":"t"}`,
		"missing token":  `{"base_url":"http://127.0.0.1:8787"}`,
		"timeout high":   `{"base_url":"http://127.0.0.1:8787","token":"t","timeout_seconds":61}`,
		"timeout low":    `{"base_url":"http://127.0.0.1:8787","token":"t","timeout_seconds":-1}`,
		"attempts high":  `{"base_url":"http://127.0.0.1:8787","token":"t","max_attempts":6}`,
		"attempts low":   `{"base_url":"http://127.0.0.1:8787","token":"t","max_attempts":-1}`,
		"retry high":     `{"base_url":"http://127.0.0.1:8787","token":"t","retry_base_millis":2001}`,
		"retry negative": `{"base_url":"http://127.0.0.1:8787","token":"t","retry_base_millis":-1}`,
	}
	for name, body := range cases {
		path := writeProviderFile(t, body, 0o600)
		_, err := LoadConfigFile(path)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("%s: error leaks the token", name)
		}
	}
}
