package taskengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real production executable, no network seams in the product. Run only inside
// tools/b5-continuous.py's network-none container with its numeric local alias.
func TestB5ContinuousProductProxy(t *testing.T) {
	if os.Getenv("BBA_B5_CONTINUOUS") != "1" {
		t.Skip("explicit isolated network container required")
	}
	store := fixtureStore(t)
	task := modelTestTask(t, store, 3)
	var hits atomic.Int32
	srv, _ := b5TLSAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Proxy-Authorization") != "" || strings.Contains(string(body), "fixture-proxy-user") || strings.Contains(string(body), "fixture-proxy-password") {
			t.Error("credential reached provider")
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "fixture", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": modelTestProposal(t, task)}}}})
	}), "8.8.8.8:0")
	proxy := b5FixtureProxy(t, "http", srv.Listener.Addr().String())
	private := t.TempDir()
	os.Chmod(private, 0700)
	cert := filepath.Join(private, "ca.pem")
	if e := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.TLS.Certificates[0].Certificate[0]}), 0600); e != nil {
		t.Fatal(e)
	}
	const token = "isolated-continuous-token"
	hash := sha256.Sum256([]byte(token))
	resolver, _ := json.Marshal(map[string]any{"credentials": []any{map[string]any{"token_sha256": hex.EncodeToString(hash[:]), "workspace_id": "ws_fixture", "principal_id": "principal_fixture", "permissions": []string{"operator.read", "model.configure", "model.request", "system.read", "system.configure"}}}})
	resolverPath := filepath.Join(private, "resolver.json")
	os.WriteFile(resolverPath, resolver, 0600)
	listener := httptest.NewUnstartedServer(nil)
	origin := "http://" + listener.Listener.Addr().String()
	listener.Listener.Close()
	logPath := filepath.Join(os.Getenv("BBA_B5_CONTINUOUS_OUT"), "product.log")
	log, e := os.Create(logPath)
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	cmd := exec.Command(os.Getenv("BBA_B5_PRODUCT_BINARY"), "-listen", strings.TrimPrefix(origin, "http://"), "-origin", origin, "-resolver-file", resolverPath, "-model-registry-file", filepath.Join(private, "registry.json"))
	cmd.Env = append(os.Environ(), "BBA_TASKENGINE_DSN="+store.Pool.Config().ConnString(), "SSL_CERT_FILE="+cert)
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	pid := cmd.Process.Pid
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		e := cmd.Wait()
		if e != nil {
			t.Error("product exit", e)
		}
	}()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	call := func(method, path, body, key string) (int, []byte, error) {
		req, e := http.NewRequest(method, origin+path, strings.NewReader(body))
		if e != nil {
			return 0, nil, e
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, e := client.Do(req)
		if e != nil {
			return 0, nil, e
		}
		defer res.Body.Close()
		b, e := io.ReadAll(res.Body)
		return res.StatusCode, b, e
	}
	ready := false
	for i := 0; i < 100; i++ {
		status, _, _ := call("GET", "/v1/operator/models", "", "")
		if status == 200 {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("product not ready")
	}
	base := "https://model.bba.example:" + strings.Split(srv.Listener.Addr().String(), ":")[1] + "/v1"
	write, _ := json.Marshal(modelProfileWrite{Name: "continuous fixture", Model: "fixture", BaseURL: base, Enabled: true, IsDefault: true, TimeoutSeconds: 3, MaxConcurrent: 2, SecretAction: "replace", APIKey: "fixture-provider-key"})
	status, body, e := call("POST", "/v1/operator/models", string(write), "")
	if e != nil || status != 200 {
		t.Fatal("save model", status, string(body), e)
	}
	var result struct {
		Data ModelProfile `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Data.ID == "" {
		t.Fatal("model response")
	}
	proxyBody, _ := json.Marshal(map[string]any{"enabled": true, "url": proxy.url, "expected_version": 0, "secret_action": "replace", "username": "fixture-proxy-user", "password": "fixture-proxy-password"})
	status, body, e = call("PUT", GlobalProxyPath, string(proxyBody), "")
	if e != nil || status != 200 {
		t.Fatal("save proxy", status, e)
	}
	status, body, e = call("POST", "/v1/operator/models/"+result.Data.ID+"/test", "", "")
	if e != nil || status != 200 {
		t.Fatal("real LLM test", status, string(body), e)
	}
	request, _ := json.Marshal(modelTestRequest(task))
	status, body, e = call("POST", modelRequestPath, string(request), "continuous-plan")
	if e != nil || status != 202 {
		t.Fatal("real planning", status, string(body), e)
	}
	if hits.Load() != 2 || proxy.count.Load() != 2 || proxy.auth.Load() != 2 {
		t.Fatal("persistent process routes not proxied", hits.Load(), proxy.count.Load())
	}
	// Disable applies to the next call without restarting the same product PID.
	disabled, _ := json.Marshal(map[string]any{"enabled": false, "url": proxy.url, "expected_version": 1, "secret_action": "keep"})
	status, _, e = call("PUT", GlobalProxyPath, string(disabled), "")
	if e != nil || status != 200 {
		t.Fatal("disable", status, e)
	}
	status, _, e = call("POST", modelRequestPath, string(request), "continuous-plan-direct")
	if e != nil || status != 202 {
		t.Fatal("direct planning", status, e)
	}
	if hits.Load() != 3 || proxy.count.Load() != 2 {
		t.Fatal("next-call direct path not consumed")
	}
	record := map[string]any{"same_product_pid": pid, "real_llm_test_status": 200, "real_plan_status": 202, "provider_count": hits.Load(), "connect_count": proxy.count.Load(), "proxy_auth_count": proxy.auth.Load(), "disable_next_call_direct": true, "production_network_seam": false, "network": "none; numeric alias inside owned container only", "paid_calls": false}
	b, _ := json.MarshalIndent(record, "", "  ")
	if e = os.WriteFile(filepath.Join(os.Getenv("BBA_B5_CONTINUOUS_OUT"), "consumer.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	t.Log(string(b))
	_ = context.Background()
}
