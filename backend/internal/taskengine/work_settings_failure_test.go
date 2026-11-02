package taskengine

import (
	"bba.local/taskengine/internal/contract"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWorkSettingsFallbackSnapshotScopeAndUnavailable(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	s.WorkSlotsDefault = 3
	seedAccount(t, s, "acc_settings_failure", "rt_settings_failure", 20)
	task := workSettingsReadyTask(t, s, []string{"acc_settings_failure"}, []string{"9024401"}, 1, 2, "fixture reason", "fixture answer", fixtureActor())
	actor := fixtureActor()
	actor.Permissions = map[string]bool{"task.read": true}
	snapshot := func(want *int) {
		t.Helper()
		w := workSettingsRequest(s, actor, "GET", taskWorkSlotsPath+task.Id, "")
		var out struct {
			Data TaskWorkSlots `json:"data"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil || w.Code != 200 {
			t.Fatal(w.Code, w.Body, e)
		}
		if (out.Data.WorkSlots == nil) != (want == nil) || want != nil && *out.Data.WorkSlots != *want {
			t.Fatal(out.Data, want)
		}
	}
	snapshot(nil)
	// Same-workspace other principal without manage_all cannot inspect this task.
	stranger := actor
	stranger.PrincipalID = "stranger"
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.principal(workspace_id,id) VALUES('ws_fixture','stranger');INSERT INTO core.workspace(id) VALUES('ws_snapshot_other');INSERT INTO core.principal(workspace_id,id) VALUES('ws_snapshot_other','principal_fixture')`); e != nil {
		t.Fatal(e)
	}
	cross := actor
	cross.WorkspaceID = "ws_snapshot_other"
	for _, who := range []Actor{stranger, cross} {
		w := workSettingsRequest(s, who, "GET", taskWorkSlotsPath+task.Id, "")
		if w.Code != 404 {
			t.Fatal(w.Code, w.Body)
		}
	}
	// Missing settings table is NOT an absent settings row: start fails closed.
	if _, e := s.Pool.Exec(ctx, `ALTER TABLE core.workspace_work_settings RENAME TO settings_unavailable`); e != nil {
		t.Fatal(e)
	}
	_, e := s.ControlTask(ctx, fixtureActor(), task.Id, newID("start"), contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "unavailable settings"})
	if e == nil {
		t.Fatal("start ignored database failure")
	}
	snapshot(nil)
	got, e := s.GetTask(ctx, fixtureActor(), task.Id)
	if e != nil || got.State != "READY" || got.Version != task.Version {
		t.Fatal(got, e)
	}
	for _, method := range []string{"GET", "PUT"} {
		w := workSettingsRequest(s, workSettingsActor(), method, workSettingsPath, `{"work_slots":2,"expected_version":0}`)
		if w.Code != 503 || strings.Contains(w.Body.String(), "settings_unavailable") || strings.Contains(w.Body.String(), "relation") {
			t.Fatal(w.Code, w.Body)
		}
	}
	if _, e = s.Pool.Exec(ctx, `ALTER TABLE core.settings_unavailable RENAME TO workspace_work_settings`); e != nil {
		t.Fatal(e)
	}
	_, e = s.ControlTask(ctx, fixtureActor(), task.Id, newID("start"), contract.TaskControlRequest{Action: "start", ExpectedVersion: task.Version, Reason: "CLI fallback"})
	if e != nil {
		t.Fatal(e)
	}
	n := 3
	snapshot(&n)
	if scalarInt(t, s, `SELECT count(*) FROM core.workspace_work_settings_audit`) != 0 {
		t.Fatal("start wrote settings audit")
	}
	s.Close()
	for _, method := range []string{"GET", "PUT"} {
		w := workSettingsRequest(s, workSettingsActor(), method, workSettingsPath, `{"work_slots":2,"expected_version":0}`)
		if w.Code != 503 {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := workSettingsRequest(s, actor, "GET", taskWorkSlotsPath+task.Id, "")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestWorkSettingsAuditFailureRollsBackSave(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	a := workSettingsActor()
	if _, e := s.Pool.Exec(ctx, `ALTER TABLE core.workspace_work_settings_audit RENAME TO audit_unavailable`); e != nil {
		t.Fatal(e)
	}
	w := workSettingsRequest(s, a, "PUT", workSettingsPath, `{"work_slots":2,"expected_version":0}`)
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
	if scalarInt(t, s, `SELECT count(*) FROM core.workspace_work_settings`) != 0 {
		t.Fatal("unaudited save persisted")
	}
	if _, e := s.Pool.Exec(ctx, `ALTER TABLE core.audit_unavailable RENAME TO workspace_work_settings_audit`); e != nil {
		t.Fatal(e)
	}
	out := workSettingsSave(t, s, a, 2, 0)
	if out.Version != 1 || !out.WorkerEnabled || !out.ProposalEnabled || out.Transport != "fixture" {
		t.Fatal(out)
	}
	rejectsSQL(t, s, `UPDATE core.workspace_work_settings_audit SET new_work_slots=3`)
	rejectsSQL(t, s, `DELETE FROM core.workspace_work_settings_audit`)
	// Every response is an actor-scoped view, not a global configuration cache.
	noPermissions := Actor{WorkspaceID: a.WorkspaceID, PrincipalID: a.PrincipalID, Permissions: map[string]bool{}}
	w = workSettingsRequest(s, noPermissions, http.MethodGet, workSettingsPath, "")
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestWorkSettingsWorkspaceFirstStarts(t *testing.T) {
	s := fixtureStore(t)
	ctx := context.Background()
	a := fixtureActor()
	b := fixtureActor()
	b.WorkspaceID = "ws_settings_start_two"
	b.PrincipalID = "principal_two"
	if _, e := s.Pool.Exec(ctx, `INSERT INTO core.workspace(id) VALUES('ws_settings_start_two');INSERT INTO core.principal(workspace_id,id) VALUES('ws_settings_start_two','principal_two')`); e != nil {
		t.Fatal(e)
	}
	for i, actor := range []Actor{a, b} {
		settings := workSettingsActor()
		settings.WorkspaceID = actor.WorkspaceID
		settings.PrincipalID = actor.PrincipalID
		workSettingsSave(t, s, settings, i+1, 0)
		account := fmt.Sprintf("acc_workspace_settings_%d", i)
		b4SeedAccount(t, s, account, "rt_"+account, 20, actor)
		task := b4ReadyTask(t, s, []string{account}, []string{fmt.Sprint(9024501 + i)}, 1, 2, "fixture reason", "fixture answer", actor, nil)
		out, e := s.GetTaskWorkSlots(ctx, actor, task.Id)
		if e != nil || out.WorkSlots == nil || *out.WorkSlots != i+1 {
			t.Fatal(out, e)
		}
	}
}
