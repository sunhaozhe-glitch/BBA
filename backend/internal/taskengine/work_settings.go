package taskengine

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// WorkSettingsRuntime is a value copy of immutable startup flags, not configuration
// authority. Saving a workspace default does not enable either execution mode.
type WorkSettingsRuntime struct {
	WorkerEnabled   bool   `json:"worker_enabled"`
	ProposalEnabled bool   `json:"proposal_enabled"`
	Transport       string `json:"transport"`
}
type WorkSettings struct {
	WorkspaceID  string `json:"workspace_id"`
	WorkSlots    int    `json:"work_slots"`
	Version      int64  `json:"version"`
	Source       string `json:"source"`
	CanConfigure bool   `json:"can_configure"`
	WorkSettingsRuntime
}

// Dedicated two-int advisory key space, different from task/content guards.
// This locks even an ABSENT settings row. Both save and first start hold it
// until commit; the subsequent SELECT is a fresh READ COMMITTED statement.
func workSettingsLock(ctx context.Context, tx pgx.Tx, workspace string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(740415,hashtext($1))`, workspace)
	return err
}
func workSettingsAuthorize(ctx context.Context, tx pgx.Tx, actor Actor, permission string) error {
	if err := actor.require(permission); err != nil {
		return err
	}
	if actor.RuntimeID != "" {
		return problem(taskCodeForbidden)
	}
	return taskEnsurePrincipal(ctx, tx, actor)
}
func (s *Store) loadWorkSettings(ctx context.Context, tx pgx.Tx, workspace string) (WorkSettings, error) {
	out := WorkSettings{WorkspaceID: workspace, Source: "workspace"}
	err := tx.QueryRow(ctx, `SELECT work_slots,version FROM core.workspace_work_settings WHERE workspace_id=$1`, workspace).Scan(&out.WorkSlots, &out.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Source = "cli_fallback"
		out.WorkSlots, err = s.workSlotsDefault()
	}
	return out, err
}
func (s *Store) GetWorkSettings(ctx context.Context, actor Actor) (WorkSettings, error) {
	var out WorkSettings
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := workSettingsAuthorize(ctx, tx, actor, "system.read"); err != nil {
			return err
		}
		var err error
		out, err = s.loadWorkSettings(ctx, tx, actor.WorkspaceID)
		return err
	})
	out.CanConfigure = actor.Permissions["system.configure"]
	return out, err
}
func (s *Store) SaveWorkSettings(ctx context.Context, actor Actor, slots int, expected int64) (WorkSettings, error) {
	var out WorkSettings
	if slots < 1 || slots > MaxTaskWorkSlots || expected < 0 {
		return out, problem(taskCodeValidation)
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := workSettingsAuthorize(ctx, tx, actor, "system.configure"); err != nil {
			return err
		}
		if err := workSettingsLock(ctx, tx, actor.WorkspaceID); err != nil {
			return err
		}
		old, err := s.loadWorkSettings(ctx, tx, actor.WorkspaceID)
		if err != nil {
			return err
		}
		if old.Version != expected {
			return problem(taskCodeVersionConflict)
		}
		out = WorkSettings{WorkspaceID: actor.WorkspaceID, WorkSlots: slots, Version: old.Version + 1, Source: "workspace", CanConfigure: true}
		if _, err = tx.Exec(ctx, `INSERT INTO core.workspace_work_settings(workspace_id,work_slots,version,updated_by) VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id) DO UPDATE SET work_slots=excluded.work_slots,version=excluded.version,updated_by=excluded.updated_by,updated_at=clock_timestamp()`, actor.WorkspaceID, slots, out.Version, actor.PrincipalID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO core.workspace_work_settings_audit(workspace_id,version,principal_id,old_work_slots,new_work_slots,old_version,old_source) VALUES($1,$2,$3,$4,$5,$6,$7)`, actor.WorkspaceID, out.Version, actor.PrincipalID, old.WorkSlots, slots, old.Version, old.Source)
		return err
	})
	return out, err
}

type TaskWorkSlots struct {
	TaskID      string `json:"task_id"`
	WorkspaceID string `json:"workspace_id"`
	WorkSlots   *int   `json:"work_slots"`
}

func (s *Store) GetTaskWorkSlots(ctx context.Context, actor Actor, id string) (TaskWorkSlots, error) {
	out := TaskWorkSlots{TaskID: id, WorkspaceID: actor.WorkspaceID}
	if err := actor.require("task.read"); err != nil {
		return out, err
	}
	if actor.RuntimeID != "" {
		return out, problem(taskCodeForbidden)
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, id))
		if err != nil {
			return err
		}
		if err = authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT work_slots FROM core.task WHERE workspace_id=$1 AND id=$2`, actor.WorkspaceID, id).Scan(&out.WorkSlots)
	})
	return out, err
}
