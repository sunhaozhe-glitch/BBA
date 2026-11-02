package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20


type httpStatusError struct { method, path string; status int; body string }

func (e *httpStatusError) Error() string { return fmt.Sprintf("%s %s returned HTTP %d: %s", e.method, e.path, e.status, e.body) }

type pageMeta struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

type taskEnvelope struct {
	Data struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"data"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "bba-operator:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bba-operator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseURL := flags.String("url", "", "explicit Task Engine API origin, e.g. https://operator.example")
	tokenFile := flags.String("token-file", "", "path to a bearer token file (mode 0600 or stricter)")
	idempotencyKey := flags.String("idempotency-key", "", "optional stable key to safely replay the same mutation")
	expectedVersion := flags.Int("expected-version", 0, "fixed task version for replaying the exact same mutation")
	timeout := flags.Duration("timeout", 10*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *idempotencyKey != "" && (len(*idempotencyKey) > 128 || strings.TrimSpace(*idempotencyKey) != *idempotencyKey) {
		return errors.New("-idempotency-key must be 1..128 non-whitespace characters")
	}
	if *expectedVersion < 0 { return errors.New("-expected-version cannot be negative") }
	origin, err := validateOrigin(*baseURL)
	if err != nil {
		return err
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		return errors.New("command required: accounts | tasks | task <id> | pause <id> | cancel <id> | reconcile <id>")
	}
	client := &http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	api := operatorAPI{origin: origin, token: token, client: client, idempotencyKey: *idempotencyKey}
	switch args[0] {
	case "accounts":
		if len(args) != 1 {
			return errors.New("accounts takes no arguments")
		}
		return api.list(stdout, "/v1/accounts")
	case "tasks":
		if len(args) != 1 {
			return errors.New("tasks takes no arguments")
		}
		return api.list(stdout, "/v1/tasks")
	case "task":
		if len(args) != 2 {
			return errors.New("usage: task <id>")
		}
		var envelope map[string]any
		status, err := api.request(http.MethodGet, "/v1/tasks/"+url.PathEscape(args[1]), nil, "", &envelope)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("GET task returned HTTP %d", status)
		}
		return writeJSON(stdout, envelope)
	case "pause", "cancel", "reconcile":
		if len(args) < 3 || args[1] == "" || strings.TrimSpace(strings.Join(args[2:], " ")) == "" {
			return fmt.Errorf("usage: %s <id> <reason>", args[0])
		}
		return api.mutate(stdout, args[0], args[1], strings.Join(args[2:], " "), *expectedVersion)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type operatorAPI struct {
	origin string
	token string
	idempotencyKey string
	client *http.Client
}

func (api operatorAPI) request(method, path string, body any, key string, target any) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, api.origin+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+api.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := api.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, err
	}
	if len(data) > maxResponseBytes {
		return response.StatusCode, errors.New("server response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, &httpStatusError{method: method, path: path, status: response.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if target != nil {
		if err := json.Unmarshal(data, target); err != nil {
			return response.StatusCode, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return response.StatusCode, nil
}

func (api operatorAPI) list(stdout io.Writer, path string) error {
	var all []json.RawMessage
	cursor := ""
	totalBytes := 0
	for pages := 0; pages < 1000; pages++ {
		endpoint := path
		if cursor != "" {
			query := url.Values{"cursor": []string{cursor}}
			endpoint += "?" + query.Encode()
		}
		var response struct {
			Data struct { Items []json.RawMessage `json:"items"` } `json:"data"`
			Meta pageMeta `json:"meta"`
		}
		status, err := api.request(http.MethodGet, endpoint, nil, "", &response)
		if err != nil { return err }
		if status != http.StatusOK { return fmt.Errorf("GET %s returned HTTP %d", endpoint, status) }
		for _, item := range response.Data.Items {
			totalBytes += len(item)
			if totalBytes > maxResponseBytes { return errors.New("combined list exceeds 4 MiB output limit") }
		}
		all = append(all, response.Data.Items...)
		if !response.Meta.HasMore { return writeJSON(stdout, map[string]any{"data": map[string]any{"items": all}}) }
		if response.Meta.NextCursor == nil || *response.Meta.NextCursor == "" { return errors.New("invalid page: has_more without next_cursor") }
		cursor = *response.Meta.NextCursor
	}
	return errors.New("page limit exceeded")
}

func (api operatorAPI) mutate(stdout io.Writer, action, taskID, reason string, expectedVersion int) error {
	path := "/v1/tasks/" + url.PathEscape(taskID)
	var current taskEnvelope
	status, err := api.request(http.MethodGet, path, nil, "", &current)
	if err != nil {
		return err
	}
	if status != http.StatusOK || current.Data.ID == "" || current.Data.Version < 1 {
		return fmt.Errorf("GET task returned incomplete version projection (HTTP %d)", status)
	}
	if expectedVersion < 1 { expectedVersion = current.Data.Version }
	key := api.idempotencyKey
	if key == "" {
		var keyBytes [24]byte
		if _, err := rand.Read(keyBytes[:]); err != nil { return fmt.Errorf("create idempotency key: %w", err) }
		key = hex.EncodeToString(keyBytes[:])
	}
	var body any
	endpoint := path + "/controls"
	if action == "reconcile" {
		endpoint = path + "/reconcile"
		body = map[string]any{"expected_version": expectedVersion, "reason": reason}
	} else {
		body = map[string]any{"action": action, "expected_version": expectedVersion, "reason": reason}
	}
	var receipt map[string]any
	status, err = api.request(http.MethodPost, endpoint, body, key, &receipt)
	if err != nil {
		var statusErr *httpStatusError
		if !errors.As(err, &statusErr) { return fmt.Errorf("mutation result unknown; retry with -idempotency-key %q -expected-version %d: %w", key, expectedVersion, err) }
		return err
	}
	if status != http.StatusAccepted { return fmt.Errorf("%s returned HTTP %d, expected 202", endpoint, status) }
	if err := writeJSON(stdout, map[string]any{"http_status": status, "idempotency_key": key, "expected_version": expectedVersion, "receipt": receipt}); err != nil { return err }
	fmt.Fprintln(stdout, "accepted: this receipt confirms durable request acceptance only; it does not confirm an external action or reconciliation result")
	return nil
}

func validateOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", errors.New("-url must be an explicit http(s) origin without path, credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1" {
		return "", errors.New("non-loopback operator URLs require https")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func readToken(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat token file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("token file must be a regular file with mode 0600 or stricter")
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") || len(token) > 4096 {
		return "", errors.New("token file must contain one non-empty bearer token")
	}
	return token, nil
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
