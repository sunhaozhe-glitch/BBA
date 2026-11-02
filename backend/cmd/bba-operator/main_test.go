package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func operatorTestToken(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("fixture-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorCLIListsEveryPage(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks" {
			t.Errorf("request=%s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		if len(cursors) == 1 {
			fmt.Fprint(w, `{"data":{"items":[{"id":"task_a"}]},"meta":{"has_more":true,"next_cursor":"cursor/2"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"items":[{"id":"task_b"}]},"meta":{"has_more":false,"next_cursor":null}}`)
	}))
	defer server.Close()

	var stdout, stderr strings.Builder
	err := run([]string{"-url", server.URL, "-token-file", operatorTestToken(t), "tasks"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v; stderr=%s", err, stderr.String())
	}
	if !reflect.DeepEqual(cursors, []string{"", "cursor/2"}) {
		t.Fatalf("requested cursors=%q", cursors)
	}
	var output struct {
		Data struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Data.Items) != 2 || output.Data.Items[0].ID != "task_a" || output.Data.Items[1].ID != "task_b" {
		t.Fatalf("aggregated output=%+v", output.Data.Items)
	}
}

func TestOperatorCLIMutationPreservesReceiptAndExactReplayInputs(t *testing.T) {
	var getCount, postCount int
	var requestBodies []map[string]any
	var requestKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodGet {
			getCount++
			fmt.Fprint(w, `{"data":{"id":"task_fixture","version":9}}`)
			return
		}
		postCount++
		requestKey = r.Header.Get("Idempotency-Key")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode mutation: %v", err)
		}
		requestBodies = append(requestBodies, body)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"data":{"id":"receipt_fixture","resource_id":"task_fixture","action":"pause","state":"accepted","expected_version":7},"meta":{"request_id":"req_fixture"}}`)
	}))
	defer server.Close()

	args := []string{"-url", server.URL, "-token-file", operatorTestToken(t), "-idempotency-key", "retry-safe-key", "-expected-version", "7", "pause", "task_fixture", "operator requested stop"}
	var first, second, stderr strings.Builder
	if err := run(args, &first, &stderr); err != nil {
		t.Fatalf("first run: %v; stderr=%s", err, stderr.String())
	}
	if err := run(args, &second, &stderr); err != nil {
		t.Fatalf("replay run: %v; stderr=%s", err, stderr.String())
	}
	if getCount != 2 || postCount != 2 || requestKey != "retry-safe-key" {
		t.Fatalf("requests get=%d post=%d key=%q", getCount, postCount, requestKey)
	}
	if len(requestBodies) != 2 || !reflect.DeepEqual(requestBodies[0], requestBodies[1]) || requestBodies[0]["expected_version"] != float64(7) || requestBodies[0]["action"] != "pause" || requestBodies[0]["reason"] != "operator requested stop" {
		t.Fatalf("mutation bodies=%v", requestBodies)
	}
	for _, output := range []string{first.String(), second.String()} {
		if !strings.Contains(output, `"http_status": 202`) || !strings.Contains(output, `"id": "receipt_fixture"`) || !strings.Contains(output, `"idempotency_key": "retry-safe-key"`) || !strings.Contains(output, "does not confirm an external action") {
			t.Fatalf("missing receipt/status/acceptance boundary in output: %s", output)
		}
	}
}

func TestOperatorCLIReportsHTTPFailureWithoutReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":{"id":"task_fixture","version":3}}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"code":"VERSION_CONFLICT"}}`)
	}))
	defer server.Close()
	var stdout, stderr strings.Builder
	err := run([]string{"-url", server.URL, "-token-file", operatorTestToken(t), "-idempotency-key", "conflict-key", "-expected-version", "2", "cancel", "task_fixture", "stop"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") || !strings.Contains(err.Error(), "VERSION_CONFLICT") {
		t.Fatalf("error=%v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failure emitted a success receipt: %s", stdout.String())
	}
}

func TestOperatorCLIReportsExactRetryInputsAfterLostMutationResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":{"id":"task_fixture","version":8}}`)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack response: %v", err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	var stdout, stderr strings.Builder
	err := run([]string{"-url", server.URL, "-token-file", operatorTestToken(t), "-idempotency-key", "recover-this-key", "cancel", "task_fixture", "stop"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), `-idempotency-key "recover-this-key"`) || !strings.Contains(err.Error(), "-expected-version 8") {
		t.Fatalf("lost response error=%v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("unknown POST outcome emitted a receipt: %s", stdout.String())
	}
}

func TestOperatorCLIRequiresHTTPSOffLoopback(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://192.0.2.10", "http://[2001:db8::1]"} {
		if _, err := validateOrigin(origin); err == nil {
			t.Fatalf("accepted insecure non-loopback origin %q", origin)
		}
	}
	for _, origin := range []string{"http://127.0.0.1:18421", "http://localhost:18421", "http://[::1]:18421", "https://example.com"} {
		if _, err := validateOrigin(origin); err != nil {
			t.Fatalf("rejected allowed origin %q: %v", origin, err)
		}
	}
}

func TestOperatorCLIDoesNotFollowRedirects(t *testing.T) {
	redirects := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects++
		fmt.Fprint(w, `{"data":{"items":[]},"meta":{"has_more":false,"next_cursor":null}}`)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/tasks", http.StatusFound)
	}))
	defer server.Close()
	var stdout, stderr strings.Builder
	err := run([]string{"-url", server.URL, "-token-file", operatorTestToken(t), "tasks"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirect response error=%v", err)
	}
	if redirects != 0 || stdout.Len() != 0 {
		t.Fatalf("redirect target contacted %d times; stdout=%q", redirects, stdout.String())
	}
}

func TestOperatorExecutableListsTasksAgainstLocalFixture(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bba-operator")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks" {
			t.Errorf("request=%s %s", r.Method, r.URL.String())
		}
		fmt.Fprint(w, `{"data":{"items":[{"id":"task_executable"}]},"meta":{"has_more":false,"next_cursor":null}}`)
	}))
	defer server.Close()
	cmd := exec.Command(binary, "-url", server.URL, "-token-file", operatorTestToken(t), "tasks")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI executable: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"id": "task_executable"`) {
		t.Fatalf("CLI output=%s", output)
	}
}
