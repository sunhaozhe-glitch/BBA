package taskengine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestGoalMigrationFrom26PreservesExistingFacts(t *testing.T) {
	ctx := context.Background()
	s := fixtureDatabase(t, fixtureAdminConfig(t), "", false)
	// Freeze the supported predecessor independently of future migration tails.
	names := append([]string{"0001_core.sql"}, migrationTail[:16]...)
	for _, name := range names {
		sql, e := migrationFiles.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Pool.Exec(ctx, string(sql)); e != nil {
			t.Fatal(name, e)
		}
	}
	if scalarInt(t, s, "SELECT max(version) FROM core.schema_version") != 26 {
		t.Fatal("not predecessor")
	}
	if _, e := s.Pool.Exec(ctx, "INSERT INTO core.workspace(id) VALUES('old-goal-ws'); INSERT INTO core.principal(workspace_id,id) VALUES('old-goal-ws','old-operator')"); e != nil {
		t.Fatal(e)
	}
	before := fixtureSchemaAndRows(t, s, false)
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	after := fixtureSchemaAndRows(t, s, false)
	// Existing columns and row sets are preserved; schema_version intentionally changes.
	for key, value := range before {
		if key == "core.schema_version" || key == "columns" {
			continue
		}
		if got, ok := after[key]; !ok || got != value {
			t.Errorf("predecessor fact changed %s", key)
		}
	}
	var oldColumns, newColumns []map[string]any
	_ = json.Unmarshal([]byte(before["columns"]), &oldColumns)
	_ = json.Unmarshal([]byte(after["columns"]), &newColumns)
	for _, old := range oldColumns {
		found := false
		for _, next := range newColumns {
			if reflect.DeepEqual(old, next) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("legacy column changed %v", old)
		}
	}
	if scalarInt(t, s, "SELECT max(version) FROM core.schema_version") != SchemaHead() {
		t.Fatal("wrong head")
	}
	if scalarInt(t, s, "SELECT count(*) FROM core.goal_run") != 0 {
		t.Fatal("migration fabricated runs")
	}
	snapshot := fixtureSchemaAndRows(t, s, false)
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(snapshot, fixtureSchemaAndRows(t, s, false)) {
		t.Fatal("migration replay changed facts")
	}
}
