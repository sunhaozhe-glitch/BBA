package taskengine

// Bounded planning snapshots.
//
// A planning round binds one immutable, bounded basis: the external fact graph
// revision, the live Summary version of every account/group in task scope, the
// task version/scope/authorization revisions, the anchor set and the model and
// template versions. Candidates extracted from the external graph are marked
// "inference" and only ever enter the S1 proposal pipeline; nothing here
// dispatches, approves or executes.
//
// Staleness: any dependency that moved after the snapshot was taken (graph
// revision, a newer live Summary version, task version, scope revision,
// authorization revision, a vanished anchor) invalidates the snapshot. Rounds
// are capped per window and every snapshot has a deadline, so a graph that keeps
// changing cannot make the planner loop forever.
//
// The validator is exported through S1's RegisterGraphSnapshotValidator seam and
// is called inside S1's proposal/materialize transaction (which holds the task
// lock), so this file only reads the task, root, summaries and anchors.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"bba.local/taskengine/internal/artex"
	"github.com/jackc/pgx/v5"
)

// GraphPlanSummaryRef is one Summary version a snapshot was built from.
type GraphPlanSummaryRef struct {
	AccountID      string `json:"account_id"`
	GroupID        string `json:"group_id"`
	SummaryID      string `json:"summary_id"`
	SummaryVersion int    `json:"summary_version"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// GraphPlanCandidate is one external suggestion mapped onto a local target. It
// is never an action: S1 must revalidate scope, eligibility and authorization.
type GraphPlanCandidate struct {
	CandidateID      string   `json:"candidate_id"`
	ExternalNodeID   string   `json:"external_node_id"`
	TargetExternalID string   `json:"target_external_id"`
	Text             string   `json:"text"`
	Provenance       string   `json:"provenance"`
	EvidenceRefs     []string `json:"evidence_refs"`
}

// GraphPlanningSnapshot is the immutable planning basis returned to S1 and the
// console.
type GraphPlanningSnapshot struct {
	SnapshotID            string                `json:"snapshot_id"`
	WorkspaceID           string                `json:"workspace_id"`
	TaskID                string                `json:"task_id"`
	GraphKind             string                `json:"graph_kind"`
	Provider              string                `json:"provider"`
	GraphRevision         int64                 `json:"graph_revision"`
	TaskVersion           int                   `json:"task_version"`
	ScopeRevision         int                   `json:"scope_revision"`
	AuthorizationRevision int                   `json:"authorization_revision"`
	ModelVersion          string                `json:"model_version"`
	TemplateVersion       string                `json:"template_version"`
	SummaryRefs           []GraphPlanSummaryRef `json:"summary_refs"`
	AnchorIDs             []string              `json:"anchor_ids"`
	Candidates            []GraphPlanCandidate  `json:"candidates"`
	Round                 int                   `json:"round"`
	MaxRounds             int                   `json:"max_rounds"`
	CreatedAt             time.Time             `json:"created_at"`
	ExpiresAt             time.Time             `json:"expires_at"`
	Digest                string                `json:"digest"`
	State                 string                `json:"state"`
}

type graphPlanBasis struct {
	task           TaskRow
	root           graphRootRow
	summaries      []GraphPlanSummaryRef
	anchors        []string
	anchorsByGroup map[string][]string
}

// CreateGraphPlanningSnapshot builds one bounded snapshot. The external read
// happens outside any transaction; the snapshot row is written only after the
// basis is revalidated, so a pull that landed in between is reported as stale
// instead of producing a snapshot with a mismatched revision.
func (s *Store) CreateGraphPlanningSnapshot(ctx context.Context, actor Actor, taskID, key string, provider GraphSyncProvider) (GraphPlanningSnapshot, error) {
	if actor.RuntimeID != "" {
		return GraphPlanningSnapshot{}, problem(taskCodeForbidden)
	}
	if err := actor.require("task.control"); err != nil {
		return GraphPlanningSnapshot{}, err
	}
	if err := taskCheckIdempotencyKey(key); err != nil {
		return GraphPlanningSnapshot{}, err
	}
	if !taskValidID(taskID) {
		return GraphPlanningSnapshot{}, problem(taskCodeNotFound)
	}
	if provider == nil || !provider.Wired() {
		return GraphPlanningSnapshot{}, problem(graphSyncCodeNotConfigured)
	}
	path := "/v1/tasks/" + taskID + "/graph-sync/planning-snapshots"
	bodyDigest, err := digest(struct {
		TaskID string `json:"task_id"`
	}{taskID})
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	// Fast replay path: a repeated request must return the stored snapshot and
	// must not spend a planning round or call ARTEX again.
	replay, err := s.readPlanningSnapshotReplay(ctx, actor, path, key, bodyDigest, taskID)
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	if replay != nil {
		return *replay, nil
	}

	basis, err := s.readPlanningBasis(ctx, actor, taskID)
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	graph, err := provider.Graph(ctx, basis.root.ExternalRoot)
	if err != nil {
		code, _ := graphSyncErrorCode(err)
		return GraphPlanningSnapshot{}, problem(code)
	}
	targets, err := s.taskTargetExternalIDs(ctx, actor.WorkspaceID, taskID)
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	candidates := graphCandidates(basis.task, targets, basis.anchorsByGroup, graph)

	var out GraphPlanningSnapshot
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,hashtext($2))`, int32(graphSyncLockNamespace), graphAdvisoryKey(actor.WorkspaceID, taskID)); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil {
			return err
		}
		if found {
			if rec.digest != bodyDigest {
				return problem(taskCodeIdempotency)
			}
			if err := json.Unmarshal(rec.response, &out); err != nil {
				return problem(taskCodeStoreUnavailable)
			}
			return nil
		}
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.control"); err != nil {
			return err
		}
		root, bound, err := loadGraphRoot(ctx, tx, task.WorkspaceID, task.ID, graphSyncKindFact)
		if err != nil {
			return err
		}
		if !bound {
			return problem(graphSyncCodeRootNotBound)
		}
		summaries, err := latestSummaryRefs(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		anchors, err := anchorIDs(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		if root.GraphRevision != basis.root.GraphRevision ||
			task.Version != basis.task.Version ||
			task.ScopeRevision != basis.task.ScopeRevision ||
			task.AuthorizationRevision != basis.task.AuthorizationRevision ||
			!sameSummaryRefs(summaries, basis.summaries) ||
			!sameStrings(anchors, basis.anchors) {
			return problem(graphSyncCodeSnapshotStale)
		}
		var rounds int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM core.graph_planning_snapshots
			WHERE workspace_id=$1 AND task_id=$2 AND created_at>clock_timestamp()-make_interval(secs => $3::double precision)`,
			task.WorkspaceID, task.ID, graphSyncSnapshotWindow.Seconds()).Scan(&rounds); err != nil {
			return err
		}
		if rounds >= graphSyncSnapshotRounds {
			return problem(graphSyncCodePlanningBudget)
		}
		now := time.Now().UTC()
		expires := now.Add(graphSyncSnapshotTTL)
		if task.Deadline.Before(expires) {
			expires = task.Deadline.UTC()
		}
		if !expires.After(now) {
			return problem(taskCodeStateConflict)
		}
		snapshot := GraphPlanningSnapshot{
			SnapshotID:            newID("gsnap"),
			WorkspaceID:           task.WorkspaceID,
			TaskID:                task.ID,
			GraphKind:             graphSyncKindFact,
			Provider:              graphSyncProviderArtex,
			GraphRevision:         root.GraphRevision,
			TaskVersion:           task.Version,
			ScopeRevision:         task.ScopeRevision,
			AuthorizationRevision: task.AuthorizationRevision,
			ModelVersion:          graphSyncPlanningModel,
			TemplateVersion:       graphSyncPlanningTemplate,
			SummaryRefs:           summaries,
			AnchorIDs:             anchors,
			Candidates:            candidates,
			Round:                 rounds + 1,
			MaxRounds:             graphSyncSnapshotRounds,
			CreatedAt:             now,
			ExpiresAt:             expires,
			State:                 "open",
		}
		snapshot.Digest, err = graphPlanningDigest(snapshot)
		if err != nil {
			return err
		}
		summaryJSON, err := json.Marshal(snapshot.SummaryRefs)
		if err != nil {
			return err
		}
		anchorJSON, err := json.Marshal(snapshot.AnchorIDs)
		if err != nil {
			return err
		}
		candidateJSON, err := json.Marshal(snapshot.Candidates)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO core.graph_planning_snapshots(
				workspace_id,task_id,snapshot_id,graph_kind,provider,graph_revision,
				task_version,scope_revision,authorization_revision,model_version,template_version,
				summary_refs,anchor_ids,candidates,candidate_count,round,max_rounds,digest,state,created_at,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			snapshot.WorkspaceID, snapshot.TaskID, snapshot.SnapshotID, snapshot.GraphKind, snapshot.Provider,
			snapshot.GraphRevision, snapshot.TaskVersion, snapshot.ScopeRevision, snapshot.AuthorizationRevision,
			snapshot.ModelVersion, snapshot.TemplateVersion, summaryJSON, anchorJSON, candidateJSON,
			len(snapshot.Candidates), snapshot.Round, snapshot.MaxRounds, snapshot.Digest, snapshot.State,
			snapshot.CreatedAt, snapshot.ExpiresAt); err != nil {
			return err
		}
		out = snapshot
		return taskWriteIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key, bodyDigest, snapshot.SnapshotID, snapshot)
	})
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	return out, nil
}

// readPlanningSnapshotReplay reads a stored idempotency record without opening a
// write transaction. A digest mismatch still fails, so a key reuse is never
// silently answered with another request's snapshot.
func (s *Store) readPlanningSnapshotReplay(ctx context.Context, actor Actor, path, key, bodyDigest, taskID string) (*GraphPlanningSnapshot, error) {
	var out *GraphPlanningSnapshot
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := taskEnsurePrincipal(ctx, tx, actor); err != nil {
			return err
		}
		rec, found, err := taskReadIdempotency(ctx, tx, actor.WorkspaceID, actor.PrincipalID, "POST", path, key)
		if err != nil || !found {
			return err
		}
		if rec.digest != bodyDigest {
			return problem(taskCodeIdempotency)
		}
		var snapshot GraphPlanningSnapshot
		if err := json.Unmarshal(rec.response, &snapshot); err != nil {
			return problem(taskCodeStoreUnavailable)
		}
		if snapshot.TaskID != taskID {
			return problem(taskCodeIdempotency)
		}
		out = &snapshot
		return nil
	})
	return out, err
}

func (s *Store) readPlanningBasis(ctx context.Context, actor Actor, taskID string) (graphPlanBasis, error) {
	var basis graphPlanBasis
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.control"); err != nil {
			return err
		}
		root, bound, err := loadGraphRoot(ctx, tx, task.WorkspaceID, task.ID, graphSyncKindFact)
		if err != nil {
			return err
		}
		if !bound {
			return problem(graphSyncCodeRootNotBound)
		}
		if task.Deadline.Before(time.Now().UTC()) {
			return problem(taskCodeStateConflict)
		}
		summaries, err := latestSummaryRefs(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		anchors, anchorsByGroup, err := anchorIDsAndGroups(ctx, tx, task.WorkspaceID, task.ID)
		if err != nil {
			return err
		}
		basis = graphPlanBasis{task: task, root: root, summaries: summaries, anchors: anchors, anchorsByGroup: anchorsByGroup}
		return nil
	})
	return basis, err
}

// anchorIDsAndGroups returns the bounded anchor set plus each anchor's group, so
// candidate evidence cites the anchor of the exact target it names instead of an
// unrelated one.
func anchorIDsAndGroups(ctx context.Context, tx pgx.Tx, workspace, taskID string) ([]string, map[string][]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT anchor_id,group_id FROM core.task_graph_anchors
		WHERE workspace_id=$1 AND task_id=$2 AND graph_kind=$3 AND provider=$4
		ORDER BY anchor_id LIMIT $5`,
		workspace, taskID, graphSyncKindFact, graphSyncProviderArtex, graphSyncMaxAnchorIDs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	byGroup := map[string][]string{}
	for rows.Next() {
		var id, group string
		if err := rows.Scan(&id, &group); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		byGroup[group] = append(byGroup[group], id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.Strings(ids)
	return ids, byGroup, nil
}

// taskTargetExternalIDs lists the QQ group ids this task is allowed to address.
// Candidate extraction is filtered by this set, so an external graph can never
// introduce a target outside the task scope.
func (s *Store) taskTargetExternalIDs(ctx context.Context, workspace, taskID string) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.external_id
		FROM core.task_target tt
		JOIN core.target t ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id
		WHERE tt.workspace_id=$1 AND tt.task_id=$2`, workspace, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var external string
		if err := rows.Scan(&external); err != nil {
			return nil, err
		}
		out[external] = true
	}
	return out, rows.Err()
}

// graphCandidates extracts bounded, target-scoped suggestions. A node whose text
// names a group outside the task target pool produces no candidate: external text
// never widens scope.
func graphCandidates(task TaskRow, targets map[string]bool, anchorsByGroup map[string][]string, graph artex.Graph) []GraphPlanCandidate {
	out := make([]GraphPlanCandidate, 0)
	seen := map[string]bool{}
	for _, node := range graph.Nodes {
		if node.Kind != artex.KindIntent && node.Kind != artex.KindGoal {
			continue
		}
		if node.State != "open" {
			continue
		}
		text := graphNodeText(node)
		if text == "" {
			continue
		}
		target := matchTarget(text, targets)
		if target == "" {
			continue
		}
		key := fmt.Sprintf("%d:%s", node.ID, target)
		if seen[key] {
			continue
		}
		seen[key] = true
		// Evidence is the trusted anchor of the target the candidate names, never
		// an unrelated anchor and never external text. A target without a Summary
		// anchor yields an empty (not fabricated) evidence list.
		evidence := make([]string, 0, 3)
		for _, anchor := range anchorsByGroup[target] {
			if len(evidence) >= 3 {
				break
			}
			evidence = append(evidence, anchor)
		}
		out = append(out, GraphPlanCandidate{
			CandidateID:      graphID("cand", task.WorkspaceID, task.ID, key),
			ExternalNodeID:   fmt.Sprint(node.ID),
			TargetExternalID: target,
			Text:             text,
			Provenance:       "inference",
			EvidenceRefs:     evidence,
		})
		if len(out) >= graphSyncMaxCandidates {
			break
		}
	}
	return out
}

// graphNodeText reads the human-readable text of one external node. Payload is
// opaque JSON, so known text fields are preferred and a bounded raw fallback is
// used only so the operator can see what was suggested.
func graphNodeText(node artex.Node) string {
	if len(node.Payload) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		return ""
	}
	for _, field := range []string{"text", "summary", "title", "goal", "description"} {
		if value, ok := payload[field].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				if len(trimmed) > 500 {
					trimmed = trimmed[:500]
				}
				return trimmed
			}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	if len(encoded) > 500 {
		encoded = encoded[:500]
	}
	return string(encoded)
}

// matchTarget finds exactly one target id mentioned by the text. Two mentions are
// ambiguous and produce no candidate.
func matchTarget(text string, targets map[string]bool) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && r != '_' && r != '-'
	})
	matches := map[string]bool{}
	for _, field := range fields {
		if targets[field] {
			matches[field] = true
		}
	}
	if len(matches) != 1 {
		return ""
	}
	for match := range matches {
		return match
	}
	return ""
}

func graphPlanningDigest(snapshot GraphPlanningSnapshot) (string, error) {
	copy := snapshot
	copy.Digest = ""
	copy.State = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	return hashBytes(encoded), nil
}

// GraphSnapshotProposalCandidates maps a snapshot's target_external_id
// candidates onto S1's local target ids for CreatePlanProposalFromSnapshot.
//
// It validates the snapshot first (stale/expired fail closed), then resolves each
// target_external_id through this task's own core.task_target row with
// eligibility='eligible'. A candidate with no local eligible target is dropped:
// external text never widens scope and never creates a target. Order is the
// snapshot order and duplicates collapse onto the first local target.
func (s *Store) GraphSnapshotProposalCandidates(ctx context.Context, actor Actor, taskID, snapshotID string) ([]GraphProposalCandidate, error) {
	if actor.RuntimeID != "" {
		return nil, problem(taskCodeForbidden)
	}
	if err := actor.require("task.read"); err != nil {
		return nil, err
	}
	if !taskValidID(taskID) || !taskValidID(snapshotID) {
		return nil, problem(taskCodeNotFound)
	}
	out := make([]GraphProposalCandidate, 0)
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		if err := validateGraphPlanningSnapshotTx(ctx, tx, actor.WorkspaceID, taskID, snapshotID); err != nil {
			return err
		}
		snapshot, err := loadPlanningSnapshot(ctx, tx, actor.WorkspaceID, taskID, snapshotID)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, candidate := range snapshot.Candidates {
			var targetID string
			err := tx.QueryRow(ctx, `
				SELECT tt.target_id
				FROM core.task_target tt
				JOIN core.target t
				  ON t.workspace_id=tt.workspace_id AND t.id=tt.target_id
				WHERE tt.workspace_id=$1 AND tt.task_id=$2
				  AND t.external_id=$3 AND tt.eligibility='eligible'`,
				actor.WorkspaceID, taskID, candidate.TargetExternalID).Scan(&targetID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if seen[targetID] {
				continue
			}
			seen[targetID] = true
			out = append(out, GraphProposalCandidate{TargetID: targetID, ExternalID: candidate.TargetExternalID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetGraphPlanningSnapshot authorizes and returns one snapshot.
func (s *Store) GetGraphPlanningSnapshot(ctx context.Context, actor Actor, taskID, snapshotID string) (GraphPlanningSnapshot, error) {
	if actor.RuntimeID != "" {
		return GraphPlanningSnapshot{}, problem(taskCodeForbidden)
	}
	if err := actor.require("task.read"); err != nil {
		return GraphPlanningSnapshot{}, err
	}
	if !taskValidID(taskID) || !taskValidID(snapshotID) {
		return GraphPlanningSnapshot{}, problem(taskCodeNotFound)
	}
	var out GraphPlanningSnapshot
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		out, err = loadPlanningSnapshot(ctx, tx, actor.WorkspaceID, taskID, snapshotID)
		return err
	})
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	return out, nil
}

// ListGraphPlanningSnapshots returns the newest bounded window of snapshots.
func (s *Store) ListGraphPlanningSnapshots(ctx context.Context, actor Actor, taskID string, limit int) ([]GraphPlanningSnapshot, error) {
	if actor.RuntimeID != "" {
		return nil, problem(taskCodeForbidden)
	}
	if err := actor.require("task.read"); err != nil {
		return nil, err
	}
	if !taskValidID(taskID) {
		return nil, problem(taskCodeNotFound)
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	var out []GraphPlanningSnapshot
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT snapshot_id FROM core.graph_planning_snapshots
			WHERE workspace_id=$1 AND task_id=$2
			ORDER BY created_at DESC LIMIT $3`, actor.WorkspaceID, taskID, limit)
		if err != nil {
			return err
		}
		ids := make([]string, 0, limit)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			snapshot, err := loadPlanningSnapshot(ctx, tx, actor.WorkspaceID, taskID, id)
			if err != nil {
				return err
			}
			out = append(out, snapshot)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func loadPlanningSnapshot(ctx context.Context, tx pgx.Tx, workspace, taskID, snapshotID string) (GraphPlanningSnapshot, error) {
	var snapshot GraphPlanningSnapshot
	var summaryJSON, anchorJSON, candidateJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT snapshot_id,graph_kind,provider,graph_revision,task_version,scope_revision,
		       authorization_revision,model_version,template_version,summary_refs,anchor_ids,candidates,
		       round,max_rounds,digest,state,created_at,expires_at
		FROM core.graph_planning_snapshots
		WHERE workspace_id=$1 AND task_id=$2 AND snapshot_id=$3`,
		workspace, taskID, snapshotID).Scan(
		&snapshot.SnapshotID, &snapshot.GraphKind, &snapshot.Provider, &snapshot.GraphRevision,
		&snapshot.TaskVersion, &snapshot.ScopeRevision, &snapshot.AuthorizationRevision,
		&snapshot.ModelVersion, &snapshot.TemplateVersion, &summaryJSON, &anchorJSON, &candidateJSON,
		&snapshot.Round, &snapshot.MaxRounds, &snapshot.Digest, &snapshot.State,
		&snapshot.CreatedAt, &snapshot.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GraphPlanningSnapshot{}, problem(taskCodeNotFound)
	}
	if err != nil {
		return GraphPlanningSnapshot{}, err
	}
	snapshot.WorkspaceID = workspace
	snapshot.TaskID = taskID
	if len(summaryJSON) > 0 {
		if err := json.Unmarshal(summaryJSON, &snapshot.SummaryRefs); err != nil {
			return GraphPlanningSnapshot{}, err
		}
	}
	if len(anchorJSON) > 0 {
		if err := json.Unmarshal(anchorJSON, &snapshot.AnchorIDs); err != nil {
			return GraphPlanningSnapshot{}, err
		}
	}
	if len(candidateJSON) > 0 {
		if err := json.Unmarshal(candidateJSON, &snapshot.Candidates); err != nil {
			return GraphPlanningSnapshot{}, err
		}
	}
	return snapshot, nil
}

// ValidateGraphPlanningSnapshot is the operator/API read validation path.
func (s *Store) ValidateGraphPlanningSnapshot(ctx context.Context, actor Actor, taskID, snapshotID string) error {
	if actor.RuntimeID != "" {
		return problem(taskCodeForbidden)
	}
	if err := actor.require("task.read"); err != nil {
		return err
	}
	if !taskValidID(taskID) || !taskValidID(snapshotID) {
		return problem(taskCodeNotFound)
	}
	var validationErr error
	err := s.tx(ctx, func(tx pgx.Tx) error {
		task, err := lockTask(ctx, tx, actor.WorkspaceID, taskID)
		if err != nil {
			return err
		}
		if err := authorizeTask(ctx, tx, actor, task, "task.read"); err != nil {
			return err
		}
		validationErr = validateGraphPlanningSnapshotTx(ctx, tx, actor.WorkspaceID, taskID, snapshotID)
		if validationErr != nil && Code(validationErr) != graphSyncCodeSnapshotStale && Code(validationErr) != graphSyncCodeSnapshotExpired {
			return validationErr
		}
		return nil
	})
	if err != nil {
		return err
	}
	return validationErr
}

// validateGraphPlanningSnapshotTx is the S1 seam: it runs inside the caller's
// transaction (which already locked the task) and only reads the snapshot, task,
// root, summaries and anchors.
func validateGraphPlanningSnapshotTx(ctx context.Context, tx pgx.Tx, workspace, taskID, snapshotID string) error {
	return validateGraphPlanningSnapshotTxVersion(ctx, tx, workspace, taskID, snapshotID, false)
}

func validateGraphPlanningSnapshotWorkItemTx(ctx context.Context, tx pgx.Tx, workspace, taskID, snapshotID, proposalID, materializationID, workItemID string) error {
	var associated bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM core.graph_planning_snapshots s
			JOIN core.plan_proposal p ON p.workspace_id=s.workspace_id AND p.task_id=s.task_id
				AND p.graph_snapshot_id=s.snapshot_id
			JOIN core.work_materialization m ON m.workspace_id=p.workspace_id AND m.task_id=p.task_id
				AND m.proposal_id=p.id
			JOIN core.work_item w ON w.workspace_id=m.workspace_id AND w.task_id=m.task_id
				AND w.materialization_id=m.id AND w.proposal_id=p.id
			JOIN core.task t ON t.workspace_id=s.workspace_id AND t.id=s.task_id
			WHERE s.workspace_id=$1 AND s.task_id=$2 AND s.snapshot_id=$3
			  AND p.id=$4 AND m.id=$5 AND w.id=$6
			  AND p.state='applied' AND p.expected_task_version=s.task_version
			  AND p.scope_revision=s.scope_revision AND p.digest=m.proposal_digest
			  AND p.approval_id=m.approval_id AND m.task_version=s.task_version
			  AND m.authorization_revision=s.authorization_revision
			  AND w.authorization_version=s.authorization_revision
			  AND t.version=s.task_version+1 AND t.scope_revision=s.scope_revision
			  AND t.authorization_revision=s.authorization_revision)`,
		workspace, taskID, snapshotID, proposalID, materializationID, workItemID).Scan(&associated)
	if err != nil {
		return err
	}
	if !associated {
		return problem(taskCodeStateConflict)
	}
	return validateGraphPlanningSnapshotTxVersion(ctx, tx, workspace, taskID, snapshotID, true)
}

func validateGraphPlanningSnapshotTxVersion(ctx context.Context, tx pgx.Tx, workspace, taskID, snapshotID string, materializedVersion bool) error {
	snapshot, err := loadPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID)
	if err != nil {
		return err
	}
	if snapshot.State == "stale" && !materializedVersion {
		return problem(graphSyncCodeSnapshotStale)
	}
	if snapshot.State == "expired" {
		return problem(graphSyncCodeSnapshotExpired)
	}
	now := time.Now().UTC()
	if !snapshot.ExpiresAt.After(now) {
		markPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID, "expired")
		return problem(graphSyncCodeSnapshotExpired)
	}
	task, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM core.task WHERE workspace_id=$1 AND id=$2`, workspace, taskID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(taskCodeNotFound)
		}
		return err
	}
	versionMatches := task.Version == snapshot.TaskVersion ||
		(materializedVersion && task.Version == snapshot.TaskVersion+1)
	if !versionMatches || task.ScopeRevision != snapshot.ScopeRevision || task.AuthorizationRevision != snapshot.AuthorizationRevision {
		markPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID, "stale")
		return problem(graphSyncCodeSnapshotStale)
	}
	root, bound, err := loadGraphRoot(ctx, tx, workspace, taskID, snapshot.GraphKind)
	if err != nil {
		return err
	}
	if !bound || root.GraphRevision != snapshot.GraphRevision {
		markPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID, "stale")
		return problem(graphSyncCodeSnapshotStale)
	}
	current, err := latestSummaryRefs(ctx, tx, workspace, taskID)
	if err != nil {
		return err
	}
	for _, ref := range snapshot.SummaryRefs {
		if !summaryRefCurrent(current, ref) {
			markPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID, "stale")
			return problem(graphSyncCodeSnapshotStale)
		}
	}
	anchors, err := anchorIDs(ctx, tx, workspace, taskID)
	if err != nil {
		return err
	}
	for _, anchor := range snapshot.AnchorIDs {
		if !slices.Contains(anchors, anchor) {
			markPlanningSnapshot(ctx, tx, workspace, taskID, snapshotID, "stale")
			return problem(graphSyncCodeSnapshotStale)
		}
	}
	return nil
}

// summaryRefCurrent reports whether the snapshot's Summary version is still the
// newest live version for its account/group.
func summaryRefCurrent(current []GraphPlanSummaryRef, ref GraphPlanSummaryRef) bool {
	for _, candidate := range current {
		if candidate.AccountID != ref.AccountID || candidate.GroupID != ref.GroupID {
			continue
		}
		return candidate.SummaryID == ref.SummaryID && candidate.SummaryVersion == ref.SummaryVersion
	}
	return false
}

func markPlanningSnapshot(ctx context.Context, tx pgx.Tx, workspace, taskID, snapshotID, state string) {
	_, _ = tx.Exec(ctx, `
		UPDATE core.graph_planning_snapshots SET state=$4
		WHERE workspace_id=$1 AND task_id=$2 AND snapshot_id=$3 AND state='open'`,
		workspace, taskID, snapshotID, state)
}

func sameSummaryRefs(a, b []GraphPlanSummaryRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// init registers the snapshot validator with S1's proposal seam. S1 fails closed
// when an id is supplied and no validator is registered, so a missing
// registration is a capability error, never a silent pass.
func init() {
	RegisterGraphSnapshotValidator(validateGraphPlanningSnapshotTx)
	RegisterGraphSnapshotWorkItemValidator(validateGraphPlanningSnapshotWorkItemTx)
}
