package taskengine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestB5OwnedOutputBoundary(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "independent-review")
	private := filepath.Join(out, "private")
	if e := os.MkdirAll(private, 0700); e != nil {
		t.Fatal(e)
	}
	nonce := strings.Repeat("a", 64)
	dsn := "owned-fixture"
	owner, _ := json.Marshal(map[string]string{"nonce": nonce, "dsn": dsn})
	for name, data := range map[string][]byte{"fixture-owner.json": owner, "registry.json": []byte("{}")} {
		if e := os.WriteFile(filepath.Join(private, name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := b5ValidateOwnedOut(root, out, dsn, nonce); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{root, out + "/../independent-review", filepath.Dir(root), "relative"} {
		if b5ValidateOwnedOut(root, p, dsn, nonce) == nil {
			t.Fatal("accepted invalid root/path", p)
		}
	}
	if b5ValidateOwnedOut(root, out, dsn, strings.Repeat("b", 64)) == nil {
		t.Fatal("accepted wrong owner")
	}
	link := filepath.Join(root, "alias")
	if e := os.Symlink(out, link); e != nil {
		t.Fatal(e)
	}
	if b5ValidateOwnedOut(root, link, dsn, nonce) == nil {
		t.Fatal("accepted symlink")
	}
	os.Chmod(filepath.Join(private, "registry.json"), 0644)
	if b5ValidateOwnedOut(root, out, dsn, nonce) == nil {
		t.Fatal("accepted unprotected registry")
	}
}

func TestB5PersistFailureAndLegacyPreservation(t *testing.T) {
	for _, stage := range []string{"file-sync", "rename", "directory-open", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			r := b5Registry(t, nil)
			mw := modelProfileWrite{Name: "legacy model", Model: "fixture", BaseURL: "https://model.bba.example/v1", Enabled: true, TimeoutSeconds: 3, MaxConcurrent: 2, SecretAction: "replace", APIKey: "fixture-only-key"}
			model, e := r.save("ws", "", mw, false)
			if e != nil {
				t.Fatal(e)
			}
			skill, e := r.saveSkill("ws", "actor", "", SkillProfile{ConfigMeta: ConfigMeta{Name: "legacy skill", Enabled: true}, Goal: "fixture", Steps: []string{"step"}, StopConditions: []string{"stop"}, Published: true}, 0, false)
			if e != nil {
				t.Fatal(e)
			}
			agent, e := r.saveAgent("ws", "actor", "", AgentProfile{ConfigMeta: ConfigMeta{Name: "legacy agent", Enabled: true}, ModelID: model.ID, Skills: []SkillBinding{{skill.ID, skill.Version}}}, 0, false)
			if e != nil {
				t.Fatal(e)
			}
			var legacy modelRegistryDisk
			legacyJSON, _ := json.Marshal(r.data)
			if e := json.Unmarshal(legacyJSON, &legacy); e != nil {
				t.Fatal(e)
			}
			path := r.path
			r.Close()
			raw, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if bytes.Contains(raw, []byte("global_proxies")) {
				t.Fatal("not a legacy file")
			}
			r, e = OpenModelRegistry(path, "", nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			if !reflect.DeepEqual(legacy, r.data) {
				t.Fatal("legacy reopen lost fields")
			}
			b5Save(t, r, "existing", true, "http://127.0.0.1:17890", "replace")
			before, _ := os.ReadFile(path)
			r.persistFault = func(s string) error {
				if s == stage {
					return errors.New("fixture failure")
				}
				return nil
			}
			enabled := true
			u := "http://127.0.0.1:17891"
			v := int64(0)
			_, e = r.saveProxy("new", "actor", proxyWrite{Enabled: &enabled, URL: &u, Expected: &v, Action: "clear"})
			if Code(e) != taskCodeStoreUnavailable {
				t.Fatal("failure hidden", e)
			}
			postRename := strings.HasPrefix(stage, "directory-")
			disk, _ := os.ReadFile(path)
			var d modelRegistryDisk
			if json.Unmarshal(disk, &d) != nil {
				t.Fatal("invalid disk")
			}
			memory, _ := json.Marshal(r.data)
			if !bytes.Equal(memory, disk) {
				t.Fatal("disk and memory diverged")
			}
			want := int64(0)
			if postRename {
				want = 1
			} else if !bytes.Equal(before, disk) {
				t.Fatal("pre-rename changed file")
			}
			got, e := r.GetProxySettings("new", true)
			if e != nil || got.Version != want {
				t.Fatal("GET mismatch", got, e)
			}
			if d.Proxies["existing"].Version != 1 || len(d.ProxyAudit) != int(want)+1 {
				t.Fatal("lost prior proxy/audit")
			}
			audit, _ := json.Marshal(d.ProxyAudit)
			for _, secret := range []string{"fixture-proxy-user", "fixture-proxy-password", "Proxy-Authorization", "http://"} {
				if bytes.Contains(audit, []byte(secret)) {
					t.Fatal("audit secret leak")
				}
			}
			if postRename {
				if _, e = r.saveProxy("new", "actor", proxyWrite{Enabled: &enabled, URL: &u, Expected: &v, Action: "clear"}); Code(e) != taskCodeVersionConflict {
					t.Fatal("CAS rollback", e)
				}
				r.persistFault = nil
				mw.ExpectedVersion = model.Version
				mw.SecretAction = "keep"
				mw.APIKey = ""
				if _, e = r.save("ws", model.ID, mw, false); Code(e) != taskCodeStoreUnavailable {
					t.Fatal("model write not blocked", e)
				}
				if _, e = r.save("other", "", modelProfileWrite{Name: "other", Model: "fixture", BaseURL: mw.BaseURL, Enabled: true, TimeoutSeconds: 3, MaxConcurrent: 1, SecretAction: "clear"}, false); Code(e) != taskCodeStoreUnavailable {
					t.Fatal("workspace write not blocked", e)
				}
				if _, e = r.saveAgent("ws", "actor", agent.ID, agent, agent.Version, false); Code(e) != taskCodeStoreUnavailable {
					t.Fatal("agent write not blocked", e)
				}
				if _, e = r.saveSkill("ws", "actor", skill.ID, skill, skill.Version, false); Code(e) != taskCodeStoreUnavailable {
					t.Fatal("skill write not blocked", e)
				}
				unchanged, _ := os.ReadFile(path)
				if !bytes.Equal(disk, unchanged) {
					t.Fatal("blocked write changed disk")
				}
			}
			r.Close()
			r, e = OpenModelRegistry(path, "", nil, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer r.Close()
			b5Save(t, r, "after", false, "", "clear")
			if !reflect.DeepEqual(r.data.Workspaces["ws"], legacy.Workspaces["ws"]) || !reflect.DeepEqual(r.data.Agents, legacy.Agents) || !reflect.DeepEqual(r.data.Skills, legacy.Skills) || !reflect.DeepEqual(r.data.AgentHistory, legacy.AgentHistory) || !reflect.DeepEqual(r.data.SkillHistory, legacy.SkillHistory) {
				t.Fatal("upgrade/save lost legacy assets")
			}
			if r.data.Proxies["new"].Version != want || r.data.Proxies["existing"].Version != 1 {
				t.Fatal("reopen lost committed proxy")
			}
			t.Log("failure surfaced; disk=memory=GET; CAS and audit preserved; legacy model/Agent/Skill/history retained; post-rename writes fail closed until reopen")
		})
	}
}

func TestB5SharedWritersPostRenameFailure(t *testing.T) {
	for _, writer := range []string{"model", "agent", "skill", "receipt"} {
		t.Run(writer, func(t *testing.T) {
			r := b5Registry(t, nil)
			mw := modelProfileWrite{Name: "fixture", Model: "fixture", BaseURL: "https://model.bba.example/v1", Enabled: true, TimeoutSeconds: 3, MaxConcurrent: 2, SecretAction: "replace", APIKey: "fixture-only"}
			m, e := r.save("ws", "", mw, false)
			if e != nil {
				t.Fatal(e)
			}
			a, e := r.saveAgent("ws", "actor", "", AgentProfile{ConfigMeta: ConfigMeta{Name: "fixture agent", Enabled: true}, ModelID: m.ID}, 0, false)
			if e != nil {
				t.Fatal(e)
			}
			s, e := r.saveSkill("ws", "actor", "", SkillProfile{ConfigMeta: ConfigMeta{Name: "fixture skill"}, Goal: "goal", Steps: []string{"step"}, StopConditions: []string{"stop"}}, 0, false)
			if e != nil {
				t.Fatal(e)
			}
			b5Save(t, r, "existing", true, "http://127.0.0.1:17890", "clear")
			r.persistFault = func(stage string) error {
				if stage == "directory-sync" {
					return errors.New("fixture failure")
				}
				return nil
			}
			switch writer {
			case "model":
				mw.Name = "changed"
				mw.ExpectedVersion = m.Version
				mw.SecretAction = "keep"
				mw.APIKey = ""
				_, e = r.save("ws", m.ID, mw, false)
			case "agent":
				a.Name = "changed"
				_, e = r.saveAgent("ws", "actor", a.ID, a, a.Version, false)
			case "skill":
				s.Name = "changed"
				_, e = r.saveSkill("ws", "actor", s.ID, s, s.Version, false)
			case "receipt":
				d := r.cloneConfig()
				d.Receipts["fixture"] = AgentReceipt{Agent: a, Model: m}
				e = r.persist(d)
			}
			if e == nil || !r.writeBlocked {
				t.Fatal("shared writer hid durability error")
			}
			disk, _ := os.ReadFile(r.path)
			mem, _ := json.Marshal(r.data)
			if !bytes.Equal(disk, mem) {
				t.Fatal("shared writer memory diverged")
			}
			if r.data.Proxies["existing"].Version != 1 || len(r.data.ProxyAudit) != 1 {
				t.Fatal("shared persist lost proxy")
			}
			switch writer {
			case "model":
				if r.data.Workspaces["ws"][0].Version != 2 {
					t.Fatal("model version rollback")
				}
			case "agent":
				if r.data.Agents["ws"][0].Version != 2 || len(r.data.AgentHistory[configKey("ws", a.ID)]) != 2 {
					t.Fatal("agent/history rollback")
				}
			case "skill":
				if r.data.Skills["ws"][0].Version != 2 || len(r.data.SkillHistory[configKey("ws", s.ID)]) != 2 {
					t.Fatal("skill/history rollback")
				}
			case "receipt":
				if r.data.Receipts["fixture"].Agent.ID != a.ID {
					t.Fatal("receipt rollback")
				}
			}
			r.persistFault = nil
			if e = r.persist(r.data); e == nil {
				t.Fatal("shared writer bypassed durability latch")
			}
			after, _ := os.ReadFile(r.path)
			if !bytes.Equal(disk, after) {
				t.Fatal("blocked persistence changed file")
			}
		})
	}
}

func TestB5PersistAPIErrorRedacted(t *testing.T) {
	r := b5Registry(t, nil)
	r.persistFault = func(stage string) error {
		if stage == "directory-sync" {
			return errors.New("fixture secret must never escape")
		}
		return nil
	}
	store := fixtureStore(t)
	a := modelTestActor()
	a.Permissions = map[string]bool{"system.read": true, "system.configure": true}
	h := r.ProxySettingsHandler(func(*http.Request) (Actor, error) { return a, nil }, store)
	body := `{"enabled":true,"url":"http://127.0.0.1:17890","expected_version":0,"secret_action":"replace","username":"fixture-proxy-user","password":"fixture-proxy-password"}`
	req := httptest.NewRequest("PUT", "/v1/operator/system/global-proxy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatal(rr.Code, rr.Body)
	}
	if !r.writeBlocked || r.data.Proxies[a.WorkspaceID].Version != 1 {
		t.Fatal("API did not execute injected post-rename failure")
	}
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", GlobalProxyPath, nil))
	var response struct {
		Data ProxySettings `json:"data"`
	}
	if json.Unmarshal(get.Body.Bytes(), &response) != nil || get.Code != 200 || response.Data.Version != 1 || !response.Data.HasCredentials {
		t.Fatal("API GET lost visible commit", get.Code)
	}
	cas := httptest.NewRecorder()
	stale := httptest.NewRequest("PUT", GlobalProxyPath, strings.NewReader(body))
	stale.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(cas, stale)
	if cas.Code != 409 {
		t.Fatal("API stale CAS not rejected", cas.Code)
	}
	for _, text := range []string{rr.Body.String(), get.Body.String(), cas.Body.String()} {
		for _, secret := range []string{"fixture-proxy-user", "fixture-proxy-password", "Proxy-Authorization", "fixture secret"} {
			if strings.Contains(text, secret) {
				t.Fatal("API output leaked secret")
			}
		}
	}
	for _, s := range []string{"fixture secret", "fixture-proxy-user", "fixture-proxy-password", "Proxy-Authorization"} {
		if strings.Contains(rr.Body.String(), s) {
			t.Fatal("API secret leak")
		}
	}
	t.Log("API 503 contains stable store error only; no injected error or proxy credentials")
}
