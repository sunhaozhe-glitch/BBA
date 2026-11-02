package taskengine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// Locate the actual checked-out project, not an author-name substring.
func b5ValidateLiveOut(out, dsn, nonce string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := cwd
	for {
		if _, e := os.Stat(filepath.Join(root, "tools", "b5-proxy-live.py")); e == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return fmt.Errorf("project root missing")
		}
		root = parent
	}
	return b5ValidateOwnedOut(filepath.Join(root, "evals", "results"), out, dsn, nonce)
}
func b5ValidateOwnedOut(root, out, dsn, nonce string) error {
	if !filepath.IsAbs(out) || filepath.Clean(out) != out || len(nonce) != 64 {
		return fmt.Errorf("invalid path or ownership token")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realOut, err := filepath.EvalSymlinks(out)
	if err != nil || realOut != out {
		return fmt.Errorf("output symlink refused")
	}
	rel, err := filepath.Rel(realRoot, realOut)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("outside results")
	}
	private := filepath.Join(out, "private")
	realPrivate, err := filepath.EvalSymlinks(private)
	if err != nil || realPrivate != private {
		return fmt.Errorf("private symlink refused")
	}
	for _, p := range []string{private, filepath.Join(private, "fixture-owner.json"), filepath.Join(private, "registry.json")} {
		st, e := os.Lstat(p)
		if e != nil {
			return fmt.Errorf("ownership file missing")
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Getuid()) || st.Mode().Perm()&0077 != 0 || (p == private && !st.IsDir()) || (p != private && !st.Mode().IsRegular()) {
			return fmt.Errorf("unprotected or unowned fixture")
		}
	}
	b, err := os.ReadFile(filepath.Join(private, "fixture-owner.json"))
	if err != nil {
		return err
	}
	var owner struct {
		Nonce    string `json:"nonce"`
		DSN      string `json:"dsn"`
		Provider string `json:"provider_port"`
		Proxy    string `json:"proxy_port"`
	}
	if json.Unmarshal(b, &owner) != nil || owner.Nonce != nonce || owner.DSN != dsn {
		return fmt.Errorf("fixture owner mismatch")
	}
	if owner.Provider != os.Getenv("BBA_B5_PROVIDER_PORT") || owner.Proxy != os.Getenv("BBA_B5_PROXY_PORT") {
		return fmt.Errorf("fixture port mismatch")
	}
	return nil
}

// Explicit isolated fixture consumer. Read browser-saved settings via the same
// protected registry; never writes proxy configuration. No production DNS/dial
// exception exists: this same-package seam maps public DNS/dials to local TLS.
func TestB5BrowserSavedConsumer(t *testing.T) {
	dsn := os.Getenv("BBA_B5_LIVE_DSN")
	if dsn == "" {
		t.Skip("explicit B5 product fixture required")
	}
	u, e := url.Parse(dsn)
	if e != nil || u.User == nil || os.Getenv("BBA_FIXTURE_ONLY") != "1" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Port() == "5432" || u.Path != "/bba_fixture" || u.User.Username() != "bba_fixture" {
		t.Fatal("refusing non-owned DB")
	}
	out := os.Getenv("BBA_B5_LIVE_OUT")
	if e := b5ValidateLiveOut(out, dsn, os.Getenv("BBA_B5_OWNER")); e != nil {
		t.Fatal("refusing out:", e)
	}
	s, e := Open(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	task := modelTestTask(t, s, 3)
	var calls atomic.Int32
	srv, client := b5TLSAt(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy auth to endpoint")
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "fixture", "choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": modelTestProposal(t, task)}}}})
	}), "127.0.0.1:"+os.Getenv("BBA_B5_PROVIDER_PORT"))
	target := srv.Listener.Addr().String()
	p := b5FixtureProxyAt(t, "http", target, "127.0.0.1:"+os.Getenv("BBA_B5_PROXY_PORT"))
	r, e := OpenModelRegistry(filepath.Join(out, "private", "registry.json"), "", nil, client)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	saved, e := r.GetProxySettings(task.WorkspaceId, true)
	if e != nil || !saved.Enabled || saved.URL != p.url {
		t.Fatal("not browser persisted", saved, e)
	}
	n := defaultModelNetwork()
	n.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dial := n.dial
	var direct atomic.Int32
	n.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "8.8.8.8:") {
			direct.Add(1)
			addr = target
		}
		return dial(ctx, network, addr)
	}
	r.network = &n
	items := r.list(task.WorkspaceId)
	if len(items) != 1 {
		t.Fatal("browser saved model missing")
	}
	a := modelTestActor()
	a.Permissions["operator.read"] = true
	a.Permissions["model.configure"] = true
	// The same real API handler used by the product's LLM test.
	h := r.Handler(func(*http.Request) (Actor, error) { return a, nil }, s)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/operator/models/"+items[0].ID+"/test", nil))
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body)
	}
	if _, e = s.RunPlanModelRequest(context.Background(), a, "b5-browser-plan", modelTestRequest(task), r); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 2 || p.count.Load() != 2 || direct.Load() != 0 {
		t.Fatal("browser settings not consumed", calls.Load(), p.count.Load(), direct.Load())
	}
	b, _ := json.MarshalIndent(map[string]any{"saved_version": saved.Version, "provider_count": calls.Load(), "connect_count": p.count.Load(), "direct_count": direct.Load(), "real_test_status": rr.Code, "real_planning": true, "production_fixture_exception": false}, "", "  ")
	if e = os.WriteFile(filepath.Join(out, "browser-saved-consumer.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	t.Log("browser_saved_version=" + strconv.FormatInt(saved.Version, 10) + " model_test_and_real_plan_CONNECT=2 provider=2 direct=0")
}
