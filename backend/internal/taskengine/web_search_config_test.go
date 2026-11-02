package taskengine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func b6SearchWrite(v int64, enabled bool, action, key string) searchWrite {
	url := ""
	no := false
	return searchWrite{Enabled: &enabled, Source: "brave-free", Expected: &v, Action: action, Key: key, Proxy: searchProxyWrite{Enabled: &no, URL: &url, Action: "keep"}}
}
func TestB6SearchConfigurationAndSeparatedBudgets(t *testing.T) {
	r, m, s, a := configFixture(t)
	if a.WebSearch {
		t.Fatal("old Agent preference must default off")
	}
	p, e := r.saveSearch("ws", "operator", b6SearchWrite(0, true, "replace", "fixture-search-key"))
	if e != nil || p.Version != 1 || !p.HasKey {
		t.Fatal(p, e)
	}
	if _, e = r.saveSearch("ws", "operator", b6SearchWrite(0, false, "keep", "")); Code(e) != taskCodeVersionConflict {
		t.Fatal("CAS", e)
	}
	if _, e = r.saveSearch("other", "operator", b6SearchWrite(0, true, "replace", "fixture-other-key")); e != nil {
		t.Fatal(e)
	}
	// Reproduce the exact old collision: a model workspace named search/ws must
	// not exhaust search ws. Model counters also remain untouched by searches.
	r.mu.Lock()
	r.active["search/ws"] = 2
	r.active["ws"] = 1
	snap, e := r.startSearchLocked("ws", true)
	if e != nil || r.active["search/ws"] != 2 || r.active["ws"] != 1 || r.searchActive["ws"] != 1 {
		t.Fatal("budget namespace collision", e)
	}
	_, e = r.startSearchLocked("ws", true)
	if e != nil {
		t.Fatal(e)
	}
	_, e = r.startSearchLocked("ws", true)
	if Code(e) != "SEARCH_BUDGET_EXCEEDED" {
		t.Fatal(e)
	}
	_, e = r.startSearchLocked("other", true)
	if e != nil {
		t.Fatal("workspace budget", e)
	}
	_, e = r.startSearchLocked("other", false)
	if Code(e) != "SEARCH_AGENT_DISABLED" {
		t.Fatal(e)
	}
	r.searchActive["ws"] = 1
	r.searchActive["other"] = 0
	r.mu.Unlock()
	if _, e = r.saveSearch("ws", "operator", b6SearchWrite(1, false, "keep", "")); e != nil {
		t.Fatal(e)
	}
	if !snap.Enabled || snap.Version != 1 || snap.Key != "fixture-search-key" {
		t.Fatal("admitted immutable snapshot changed")
	}
	r.mu.Lock()
	_, e = r.startSearchLocked("ws", true)
	r.mu.Unlock()
	if Code(e) != "SEARCH_DISABLED" {
		t.Fatal("new request must observe disabled", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	receipt, e := r.consumeSearch(ctx, "ws", "cancelled-admitted", "explicit query", snap)
	if Code(e) != "SEARCH_PROVIDER_CANCELLED" || receipt.ConfigVersion != 1 || receipt.Status != "SEARCH_PROVIDER_CANCELLED" {
		t.Fatal(receipt, e)
	}
	r.mu.Lock()
	if r.searchActive["ws"] != 0 || r.active["ws"] != 1 {
		t.Fatal("budget release crossed namespace")
	}
	r.mu.Unlock()
	if _, e = r.saveSearch("ws", "operator", b6SearchWrite(2, true, "clear", "")); e != nil {
		t.Fatal(e)
	}
	r.mu.Lock()
	_, e = r.startSearchLocked("ws", true)
	r.mu.Unlock()
	if Code(e) != "SEARCH_KEY_MISSING" {
		t.Fatal(e)
	}
	a.WebSearch = true
	a, e = r.saveAgent("ws", "operator", a.ID, a, a.Version, false)
	if e != nil {
		t.Fatal(e)
	}
	path := r.path
	r.Close()
	opened, e := OpenModelRegistry(path, "", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer opened.Close()
	read, e := opened.GetSearchSettings("ws", true)
	if e != nil || read.Version != 3 || read.HasKey {
		t.Fatal("restart search", read, e)
	}
	if len(opened.data.Workspaces["ws"]) != 1 || opened.data.Workspaces["ws"][0].ID != m.ID || opened.data.Skills["ws"][0].ID != s.ID || len(opened.data.AgentHistory[configKey("ws", a.ID)]) != 2 || !opened.data.Agents["ws"][0].WebSearch {
		t.Fatal("restart lost model/Agent/Skill history")
	}
}
func TestB6ConcurrentWorkspaceConfiguration(t *testing.T) {
	r, _, _, _ := configFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, ws := range []string{"ws", "other"} {
		wg.Add(1)
		go func(ws string) {
			defer wg.Done()
			for v := int64(0); v < 8; v++ {
				_, e := r.saveSearch(ws, "operator", b6SearchWrite(v, true, "replace", "fixture-"+ws))
				if e != nil {
					errs <- e
					return
				}
				r.mu.Lock()
				p, e := r.startSearchLocked(ws, true)
				r.searchActive[ws]--
				r.mu.Unlock()
				if e != nil {
					errs <- e
					return
				}
				if p.Key != "fixture-"+ws || p.Version != v+1 {
					errs <- problem(taskCodeStateConflict)
					return
				}
			}
		}(ws)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if filepath.Base(r.path) != "registry.json" {
		t.Fatal("unexpected fixture path")
	}
}
