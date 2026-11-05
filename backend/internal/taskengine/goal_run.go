package taskengine

// The goal loop owns reasoning records only. It has no Adapter, Reserve,
// dispatch permit, Chat Permit, external ARTEX client, or model HTTP client.
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const goalRounds = 4
const goalAttemptTimeout = 30 * time.Second
const goalRunTimeout = 30 * time.Minute

type GoalStart struct {
	ThreadID   string       `json:"thread_id"`
	GroupID    string       `json:"group_id"`
	Goal       string       `json:"goal"`
	PlannerRef SkillBinding `json:"planner_ref"`
	AnalystRef SkillBinding `json:"analyst_ref"`
}
type GoalMessage struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}
type GoalWindow struct {
	EventID     string        `json:"event_id"`
	GroupID     string        `json:"group_id"`
	WindowStart time.Time     `json:"window_start"`
	WindowEnd   time.Time     `json:"window_end"`
	ExpiresAt   time.Time     `json:"expires_at"`
	Messages    []GoalMessage `json:"messages"`
}
type GoalConfiguration struct {
	Planner AgentReceipt `json:"planner"`
	Analyst AgentReceipt `json:"analyst"`
}
type GoalMemory struct {
	Summary             string   `json:"summary"`
	InformationGaps     []string `json:"information_gaps"`
	ConsumedEvidenceIDs []string `json:"consumed_evidence_ids"`
	UpdatedRound        int      `json:"updated_round"`
}
type GoalAsset struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}
type GoalNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	State string `json:"state"`
}
type GoalEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type GoalAnchor struct {
	NodeID  string `json:"node_id"`
	AssetID string `json:"asset_id"`
}
type GoalGraph struct {
	Assets  []GoalAsset  `json:"assets"`
	Nodes   []GoalNode   `json:"nodes"`
	Edges   []GoalEdge   `json:"edges"`
	Anchors []GoalAnchor `json:"anchors"`
}
type GoalEvidence struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	Refs      []string  `json:"refs"`
	Round     int       `json:"round"`
	ExpiresAt time.Time `json:"expires_at"`
}
type GoalStep struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	State   string          `json:"state"`
	Attempt int             `json:"attempt"`
	Output  json.RawMessage `json:"output"`
}
type GoalRound struct {
	Number              int        `json:"number"`
	State               string     `json:"state"`
	ChangeReason        string     `json:"change_reason"`
	ConsumedEvidenceIDs []string   `json:"consumed_evidence_ids"`
	Steps               []GoalStep `json:"steps"`
}
type GoalRun struct {
	ID                    string            `json:"id"`
	ThreadID              string            `json:"thread_id"`
	GroupID               string            `json:"group_id"`
	Goal                  string            `json:"goal"`
	State                 string            `json:"state"`
	Version               int               `json:"version"`
	Round                 int               `json:"round"`
	Reason                string            `json:"reason"`
	Implementation        string            `json:"implementation"`
	MaxRounds             int               `json:"max_rounds"`
	MaxSteps              int               `json:"max_steps"`
	AttemptTimeoutSeconds int               `json:"attempt_timeout_seconds"`
	CreatedAt             time.Time         `json:"created_at"`
	DeadlineAt            time.Time         `json:"deadline_at"`
	Configuration         GoalConfiguration `json:"configuration"`
	Memory                GoalMemory        `json:"memory"`
	Graph                 GoalGraph         `json:"graph"`
	Rounds                []GoalRound       `json:"rounds"`
	Evidence              []GoalEvidence    `json:"evidence"`
}
type GoalService struct {
	store    *Store
	registry *ModelRegistry
	fixture  bool
}

func NewGoalService(s *Store, r *ModelRegistry, fixture bool) *GoalService {
	return &GoalService{s, r, fixture}
}
func goalTerminal(state string) bool {
	return state == "CONFIRMED" || state == "STOPPED" || state == "TIMED_OUT" || state == "LIMIT_REACHED"
}
func goalJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func (g *GoalService) authorize(ctx context.Context, a Actor, write bool) error {
	perm := "conversation.read"
	if write {
		perm = "conversation.write"
	}
	if a.RuntimeID != "" || !a.Permissions["conversation.read"] || !a.Permissions[perm] || a.WorkspaceID == "" || a.PrincipalID == "" {
		return problem(taskCodeForbidden)
	}
	var ok bool
	if err := g.store.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM core.principal WHERE workspace_id=$1 AND id=$2)", a.WorkspaceID, a.PrincipalID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return problem(taskCodeForbidden)
	}
	return nil
}
func (r *ModelRegistry) goalConfiguration(ws string, start GoalStart) (GoalConfiguration, error) {
	if r == nil {
		return GoalConfiguration{}, problem("AGENT_REFERENCE_UNAVAILABLE")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := func(ref SkillBinding) (AgentReceipt, error) {
		var profile AgentProfile
		live := false
		for _, a := range r.data.Agents[ws] {
			if a.ID == ref.ID {
				live = a.Enabled && !a.Deleted
			}
		}
		for _, a := range r.data.AgentHistory[configKey(ws, ref.ID)] {
			if a.Version == ref.Version {
				profile = a
			}
		}
		if !live || profile.ID == "" || !profile.Enabled || profile.Deleted {
			return AgentReceipt{}, problem("AGENT_REFERENCE_UNAVAILABLE")
		}
		receipt := AgentReceipt{Agent: profile, Skills: []SkillProfile{}}
		for _, binding := range profile.Skills {
			s, e := r.skillSnapshot(ws, binding)
			if e != nil {
				return receipt, e
			}
			receipt.Skills = append(receipt.Skills, s)
		}
		for _, m := range r.data.Workspaces[ws] {
			if m.ID == profile.ModelID && m.Enabled {
				receipt.Model = m.ModelProfile
			}
		}
		if receipt.Model.ID == "" {
			return receipt, problem("MODEL_REFERENCE_UNAVAILABLE")
		}
		return receipt, nil
	}
	p, e := snapshot(start.PlannerRef)
	if e != nil {
		return GoalConfiguration{}, e
	}
	a, e := snapshot(start.AnalystRef)
	return GoalConfiguration{p, a}, e
}
func (g *GoalService) Start(ctx context.Context, a Actor, key string, b GoalStart) (GoalRun, error) {
	if e := g.authorize(ctx, a, true); e != nil {
		return GoalRun{}, e
	}
	if !g.fixture {
		return GoalRun{}, problem("GOAL_FIXTURE_DISABLED")
	}
	if !taskValidID(b.PlannerRef.ID) || b.PlannerRef.Version < 1 || !taskValidID(b.AnalystRef.ID) || b.AnalystRef.Version < 1 || !taskValidID(b.ThreadID) || !taskValidQQID(b.GroupID) || strings.TrimSpace(b.Goal) == "" || len(b.Goal) > 1000 || key == "" || len(key) > 128 {
		return GoalRun{}, problem(taskCodeValidation)
	}
	d, _ := digest(b)
	id := newID("goal")
	e := g.store.tx(ctx, func(tx pgx.Tx) error {
		// Serialize a new command identity before there is a run row to lock.
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,735011))", a.WorkspaceID+"/"+a.PrincipalID+"/"+key); e != nil {
			return e
		}
		var old string
		err := tx.QueryRow(ctx, "SELECT run_id,digest FROM core.goal_command WHERE workspace_id=$1 AND principal_id=$2 AND key=$3", a.WorkspaceID, a.PrincipalID, key).Scan(&id, &old)
		if err == nil {
			if old != d {
				return problem(taskCodeIdempotency)
			}
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		conf, err := g.registry.goalConfiguration(a.WorkspaceID, b)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO core.operator_conversation(workspace_id,id,kind,conversation_kind,conversation_id) VALUES($1,$2,'agent','agent',$2) ON CONFLICT DO NOTHING", a.WorkspaceID, b.ThreadID)
		if err != nil {
			return err
		}
		var kind string
		if err = tx.QueryRow(ctx, "SELECT kind FROM core.operator_conversation WHERE workspace_id=$1 AND id=$2", a.WorkspaceID, b.ThreadID).Scan(&kind); err != nil {
			return err
		}
		if kind != "agent" {
			return problem("GOAL_SCOPE_MISMATCH")
		}
		asset := graphID("asset", a.WorkspaceID, b.GroupID, "qq_group")
		graph := GoalGraph{Assets: []GoalAsset{{asset, "qq_group", b.GroupID}}, Nodes: []GoalNode{{id, "goal", b.Goal, "open"}}, Edges: []GoalEdge{}, Anchors: []GoalAnchor{{id, asset}}}
		_, err = tx.Exec(ctx, `INSERT INTO core.goal_run(workspace_id,id,principal_id,thread_id,group_id,goal,state,reason,configuration,graph,deadline_at) VALUES($1,$2,$3,$4,$5,$6,'WAITING_INPUT','等待有效的合成窗口', $7,$8,clock_timestamp()+interval '30 minutes')`, a.WorkspaceID, id, a.PrincipalID, b.ThreadID, b.GroupID, b.Goal, goalJSON(conf), goalJSON(graph))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO core.goal_command(workspace_id,principal_id,key,digest,run_id) VALUES($1,$2,$3,$4,$5)", a.WorkspaceID, a.PrincipalID, key, d, id)
		return err
	})
	if e != nil {
		return GoalRun{}, e
	}
	return g.Get(ctx, a, id)
}
func goalRead(ctx context.Context, tx pgx.Tx, ws, id, principal string, lock bool) (GoalRun, error) {
	var r GoalRun
	var conf, memory, graph []byte
	query := `SELECT id,thread_id,group_id,goal,state,version,round,reason,configuration,memory,graph,created_at,deadline_at FROM core.goal_run WHERE workspace_id=$1 AND id=$2`
	args := []any{ws, id}
	if principal != "" {
		query += " AND principal_id=$3"
		args = append(args, principal)
	}
	if lock {
		query += " FOR UPDATE"
	}
	e := tx.QueryRow(ctx, query, args...).Scan(&r.ID, &r.ThreadID, &r.GroupID, &r.Goal, &r.State, &r.Version, &r.Round, &r.Reason, &conf, &memory, &graph, &r.CreatedAt, &r.DeadlineAt)
	if e != nil {
		return r, e
	}
	if json.Unmarshal(conf, &r.Configuration) != nil || json.Unmarshal(memory, &r.Memory) != nil || json.Unmarshal(graph, &r.Graph) != nil {
		return r, problem(taskCodeStoreUnavailable)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	r.DeadlineAt = r.DeadlineAt.UTC()
	r.Implementation = "deterministic_fixture"
	r.MaxRounds = goalRounds
	r.MaxSteps = goalRounds * 2
	r.AttemptTimeoutSeconds = 30
	r.Rounds = []GoalRound{}
	r.Evidence = []GoalEvidence{}
	return r, nil
}
func (g *GoalService) Get(ctx context.Context, a Actor, id string) (GoalRun, error) {
	if e := g.authorize(ctx, a, false); e != nil {
		return GoalRun{}, e
	}
	var out GoalRun
	e := g.store.tx(ctx, func(tx pgx.Tx) error {
		var e error
		out, e = goalRead(ctx, tx, a.WorkspaceID, id, a.PrincipalID, true)
		if e != nil {
			return e
		}
		rows, e := tx.Query(ctx, "SELECT number,state,change_reason,consumed_evidence_ids FROM core.goal_round WHERE workspace_id=$1 AND run_id=$2 ORDER BY number", a.WorkspaceID, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var r GoalRound
			var ids []byte
			if e = rows.Scan(&r.Number, &r.State, &r.ChangeReason, &ids); e != nil {
				rows.Close()
				return e
			}
			_ = json.Unmarshal(ids, &r.ConsumedEvidenceIDs)
			r.Steps = []GoalStep{}
			out.Rounds = append(out.Rounds, r)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		rows, e = tx.Query(ctx, "SELECT round,id,role,state,attempt,output FROM core.goal_step WHERE workspace_id=$1 AND run_id=$2 ORDER BY round,CASE role WHEN 'planner' THEN 0 ELSE 1 END", a.WorkspaceID, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var n int
			var s GoalStep
			if e = rows.Scan(&n, &s.ID, &s.Role, &s.State, &s.Attempt, &s.Output); e != nil {
				rows.Close()
				return e
			}
			if n > 0 && n <= len(out.Rounds) {
				out.Rounds[n-1].Steps = append(out.Rounds[n-1].Steps, s)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		rows, e = tx.Query(ctx, "SELECT id,kind,text,refs,round,expires_at FROM core.goal_evidence WHERE workspace_id=$1 AND run_id=$2 ORDER BY round,id", a.WorkspaceID, id)
		if e != nil {
			return e
		}
		for rows.Next() {
			var v GoalEvidence
			var refs []byte
			if e = rows.Scan(&v.ID, &v.Kind, &v.Text, &refs, &v.Round, &v.ExpiresAt); e != nil {
				rows.Close()
				return e
			}
			v.ExpiresAt = v.ExpiresAt.UTC()
			_ = json.Unmarshal(refs, &v.Refs)
			out.Evidence = append(out.Evidence, v)
		}
		e = rows.Err()
		rows.Close()
		return e
	})
	return out, e
}
func (g *GoalService) List(ctx context.Context, a Actor) ([]GoalRun, error) {
	if e := g.authorize(ctx, a, false); e != nil {
		return nil, e
	}
	rows, e := g.store.Pool.Query(ctx, "SELECT id FROM core.goal_run WHERE workspace_id=$1 AND principal_id=$2 ORDER BY created_at DESC LIMIT 50", a.WorkspaceID, a.PrincipalID)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []GoalRun{}
	for _, id := range ids {
		r, e := g.Get(ctx, a, id)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, nil
}
func validateGoalWindow(b GoalWindow, group string, now time.Time) error {
	if b.GroupID != group {
		return problem("GOAL_SCOPE_MISMATCH")
	}
	if !b.ExpiresAt.After(now) {
		return problem("WINDOW_EXPIRED")
	}
	if !taskValidID(b.EventID) || !b.WindowStart.Before(b.WindowEnd) || b.WindowEnd.After(now.Add(5*time.Second)) || b.WindowEnd.Sub(b.WindowStart) > 24*time.Hour || b.ExpiresAt.After(b.WindowEnd.Add(time.Hour)) || !b.ExpiresAt.After(b.WindowEnd) || len(b.Messages) == 0 || len(b.Messages) > 32 {
		return problem(taskCodeValidation)
	}
	seen := map[string]bool{}
	for _, m := range b.Messages {
		if !taskValidID(m.ID) || seen[m.ID] || strings.TrimSpace(m.Text) == "" || len(m.Text) > 2000 || m.At.Before(b.WindowStart) || m.At.After(b.WindowEnd) {
			return problem(taskCodeValidation)
		}
		seen[m.ID] = true
	}
	return nil
}
func (g *GoalService) Inject(ctx context.Context, a Actor, id, key string, b GoalWindow) (GoalRun, error) {
	if e := g.authorize(ctx, a, true); e != nil {
		return GoalRun{}, e
	}
	if !g.fixture {
		return GoalRun{}, problem("GOAL_FIXTURE_DISABLED")
	}
	if key == "" || len(key) > 128 {
		return GoalRun{}, problem(taskCodeValidation)
	}
	e := g.store.tx(ctx, func(tx pgx.Tx) error {
		r, e := goalRead(ctx, tx, a.WorkspaceID, id, a.PrincipalID, true)
		if e != nil {
			return e
		}
		d, _ := digest([]any{id, "fixture-window", b})
		if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,735011))", a.WorkspaceID+"/"+a.PrincipalID+"/"+key); e != nil {
			return e
		}
		var previous, commandDigest string
		e = tx.QueryRow(ctx, "SELECT run_id,digest FROM core.goal_command WHERE workspace_id=$1 AND principal_id=$2 AND key=$3", a.WorkspaceID, a.PrincipalID, key).Scan(&previous, &commandDigest)
		if e == nil {
			if previous != id || commandDigest != d {
				return problem(taskCodeIdempotency)
			}
			return nil
		}
		if e != pgx.ErrNoRows {
			return e
		}
		if e = validateGoalWindow(b, r.GroupID, time.Now()); e != nil {
			return e
		}
		eventDigest, _ := digest(b)
		var old string
		e = tx.QueryRow(ctx, "SELECT digest FROM core.goal_event WHERE workspace_id=$1 AND run_id=$2 AND id=$3", a.WorkspaceID, id, b.EventID).Scan(&old)
		if e == nil {
			if old != eventDigest {
				return problem(taskCodeIdempotency)
			}
			_, e = tx.Exec(ctx, "INSERT INTO core.goal_command(workspace_id,principal_id,key,digest,run_id) VALUES($1,$2,$3,$4,$5)", a.WorkspaceID, a.PrincipalID, key, d, id)
			return e
		}
		if e != pgx.ErrNoRows {
			return e
		}
		if goalTerminal(r.State) {
			return problem("GOAL_STATE_CONFLICT")
		}
		var queued int
		if e = tx.QueryRow(ctx, "SELECT count(*) FROM core.goal_event WHERE workspace_id=$1 AND run_id=$2", a.WorkspaceID, id).Scan(&queued); e != nil {
			return e
		}
		if queued >= 8 {
			return problem("GOAL_LIMIT_REACHED")
		}
		if _, e = tx.Exec(ctx, "INSERT INTO core.goal_event(workspace_id,run_id,id,digest,payload) VALUES($1,$2,$3,$4,$5)", a.WorkspaceID, id, b.EventID, eventDigest, goalJSON(b)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, "INSERT INTO core.goal_command(workspace_id,principal_id,key,digest,run_id) VALUES($1,$2,$3,$4,$5)", a.WorkspaceID, a.PrincipalID, key, d, id); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "UPDATE core.goal_run SET version=version+1,state=CASE WHEN state='AWAITING_CONFIRMATION' THEN 'WAITING_INPUT' ELSE state END,reason='新证据已入队，等待重新规划' WHERE workspace_id=$1 AND id=$2", a.WorkspaceID, id)
		return e
	})
	if e != nil {
		return GoalRun{}, e
	}
	return g.Get(ctx, a, id)
}
func (g *GoalService) Control(ctx context.Context, a Actor, id, action string, version int, key string) (GoalRun, error) {
	if e := g.authorize(ctx, a, true); e != nil {
		return GoalRun{}, e
	}
	if !g.fixture {
		return GoalRun{}, problem("GOAL_FIXTURE_DISABLED")
	}
	if (action != "stop" && action != "confirm") || key == "" || len(key) > 128 {
		return GoalRun{}, problem(taskCodeValidation)
	}
	e := g.store.tx(ctx, func(tx pgx.Tx) error {
		r, e := goalRead(ctx, tx, a.WorkspaceID, id, a.PrincipalID, true)
		if e != nil {
			return e
		}
		d, _ := digest([]any{id, action, version})
		if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,735011))", a.WorkspaceID+"/"+a.PrincipalID+"/"+key); e != nil {
			return e
		}
		var previous, old string
		e = tx.QueryRow(ctx, "SELECT run_id,digest FROM core.goal_command WHERE workspace_id=$1 AND principal_id=$2 AND key=$3", a.WorkspaceID, a.PrincipalID, key).Scan(&previous, &old)
		if e == nil {
			if previous != id || old != d {
				return problem(taskCodeIdempotency)
			}
			return nil
		}
		if e != pgx.ErrNoRows {
			return e
		}
		if version != r.Version || goalTerminal(r.State) || !r.DeadlineAt.After(time.Now()) {
			return problem("GOAL_STATE_CONFLICT")
		}
		state, reason := "STOPPED", "操作员停止；迟到结果仅归档，不恢复运行"
		if action == "confirm" {
			if r.State != "AWAITING_CONFIRMATION" {
				return problem("GOAL_STATE_CONFLICT")
			}
			var pending bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM core.goal_event WHERE workspace_id=$1 AND run_id=$2 AND state='pending')", a.WorkspaceID, id).Scan(&pending); e != nil {
				return e
			}
			if pending {
				return problem("GOAL_STATE_CONFLICT")
			}
			var expired bool
			if e = tx.QueryRow(ctx, "SELECT NOT EXISTS(SELECT 1 FROM core.goal_evidence WHERE workspace_id=$1 AND run_id=$2 AND round=$3) OR EXISTS(SELECT 1 FROM core.goal_evidence WHERE workspace_id=$1 AND run_id=$2 AND round=$3 AND expires_at<=clock_timestamp())", a.WorkspaceID, id, r.Round).Scan(&expired); e != nil {
				return e
			}
			if expired {
				return problem("WINDOW_EXPIRED")
			}
			state = "CONFIRMED"
			reason = "操作员确认目标达成"
		}
		if _, e = tx.Exec(ctx, "UPDATE core.goal_run SET state=$3,reason=$4,version=version+1 WHERE workspace_id=$1 AND id=$2", a.WorkspaceID, id, state, reason); e != nil {
			return e
		}
		if action == "stop" {
			if e = goalStopChildren(ctx, tx, a.WorkspaceID, id); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, "INSERT INTO core.goal_command(workspace_id,principal_id,key,digest,run_id) VALUES($1,$2,$3,$4,$5)", a.WorkspaceID, a.PrincipalID, key, d, id)
		return e
	})
	if e != nil {
		return GoalRun{}, e
	}
	return g.Get(ctx, a, id)
}
func goalStopChildren(ctx context.Context, tx pgx.Tx, ws, id string) error {
	for _, q := range []string{"UPDATE core.goal_step SET state='STOPPED' WHERE workspace_id=$1 AND run_id=$2 AND state IN ('READY','BLOCKED','RUNNING')", "UPDATE core.goal_round SET state='STOPPED' WHERE workspace_id=$1 AND run_id=$2 AND state='RUNNING'", "UPDATE core.goal_event SET state='ignored',reason='run stopped' WHERE workspace_id=$1 AND run_id=$2 AND state='pending'"} {
		if _, e := tx.Exec(ctx, q, ws, id); e != nil {
			return e
		}
	}
	return nil
}

// GoalJob is a durable logical role step with a fenced attempt and immutable input.
type GoalJob struct {
	ID          string
	WorkspaceID string
	RunID       string
	Round       int
	Role        string
	Attempt     int
	Window      GoalWindow
	Memory      GoalMemory
	Goal        string
	Plan        *goalOutput
}
type goalInput struct {
	Window GoalWindow  `json:"window"`
	Memory GoalMemory  `json:"memory"`
	Goal   string      `json:"goal"`
	Plan   *goalOutput `json:"plan,omitempty"`
}

func (g *GoalService) publish(ctx context.Context) error {
	return g.store.tx(ctx, func(tx pgx.Tx) error {
		var ws, id, event string
		var payload []byte
		e := tx.QueryRow(ctx, `SELECT r.workspace_id,r.id,e.id,e.payload FROM core.goal_run r JOIN core.goal_event e ON e.workspace_id=r.workspace_id AND e.run_id=r.id WHERE e.state='pending' AND r.state<>'RUNNING' ORDER BY e.created_at,e.id LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&ws, &id, &event, &payload)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		r, e := goalRead(ctx, tx, ws, id, "", false)
		if e != nil {
			return e
		}
		var w GoalWindow
		if json.Unmarshal(payload, &w) != nil {
			return problem(taskCodeStoreUnavailable)
		}
		state, reason := "", ""
		if goalTerminal(r.State) {
			state = "ignored"
			reason = "run is terminal"
		} else if !r.DeadlineAt.After(time.Now()) {
			state = "ignored"
			reason = "RUN_DEADLINE"
			r.State = "TIMED_OUT"
		} else if r.Round >= goalRounds {
			state = "ignored"
			reason = "ROUND_LIMIT"
			r.State = "LIMIT_REACHED"
		} else if err := validateGoalWindow(w, r.GroupID, time.Now()); err != nil {
			state = "rejected"
			reason = Code(err)
			r.State = "WAITING_INPUT"
		}
		if state != "" {
			if _, e = tx.Exec(ctx, "UPDATE core.goal_event SET state=$4,reason=$5 WHERE workspace_id=$1 AND run_id=$2 AND id=$3", ws, id, event, state, reason); e != nil {
				return e
			}
			_, e = tx.Exec(ctx, "UPDATE core.goal_run SET state=$3,reason=$4,version=version+1 WHERE workspace_id=$1 AND id=$2", ws, id, r.State, reason)
			return e
		}
		n := r.Round + 1
		if _, e = tx.Exec(ctx, "INSERT INTO core.goal_round(workspace_id,run_id,number,event_id,state) VALUES($1,$2,$3,$4,'RUNNING')", ws, id, n, event); e != nil {
			return e
		}
		in := goalJSON(goalInput{Window: w, Memory: r.Memory, Goal: r.Goal})
		for _, role := range []string{"planner", "analyst"} {
			state := "BLOCKED"
			if role == "planner" {
				state = "READY"
			}
			if _, e = tx.Exec(ctx, "INSERT INTO core.goal_step(workspace_id,run_id,round,id,role,state,input) VALUES($1,$2,$3,$4,$5,$6,$7)", ws, id, n, graphID("gstep", ws, id, fmt.Sprintf("%d:%s", n, role)), role, state, in); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, "UPDATE core.goal_event SET state='published' WHERE workspace_id=$1 AND run_id=$2 AND id=$3", ws, id, event); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, "UPDATE core.goal_run SET state='RUNNING',round=$3,reason='新窗口已发布，等待角色步骤',version=version+1 WHERE workspace_id=$1 AND id=$2", ws, id, n)
		return e
	})
}
func (g *GoalService) claim(ctx context.Context) (*GoalJob, error) {
	var out *GoalJob
	e := g.store.tx(ctx, func(tx pgx.Tx) error {
		var ws, id string
		e := tx.QueryRow(ctx, `SELECT r.workspace_id,r.id FROM core.goal_run r WHERE r.state='RUNNING' AND (r.deadline_at<=clock_timestamp() OR EXISTS(SELECT 1 FROM core.goal_step s WHERE s.workspace_id=r.workspace_id AND s.run_id=r.id AND (s.state='READY' OR (s.state='RUNNING' AND s.lease_until<=clock_timestamp())))) ORDER BY r.created_at LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&ws, &id)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		r, e := goalRead(ctx, tx, ws, id, "", false)
		if e != nil {
			return e
		}
		if !r.DeadlineAt.After(time.Now()) {
			if e = goalStopChildren(ctx, tx, ws, id); e != nil {
				return e
			}
			_, e = tx.Exec(ctx, "UPDATE core.goal_run SET state='TIMED_OUT',reason='RUN_DEADLINE',version=version+1 WHERE workspace_id=$1 AND id=$2", ws, id)
			return e
		}
		var j GoalJob
		var input []byte
		e = tx.QueryRow(ctx, `SELECT id,round,role,attempt,input FROM core.goal_step WHERE workspace_id=$1 AND run_id=$2 AND (state='READY' OR (state='RUNNING' AND lease_until<=clock_timestamp())) ORDER BY round,CASE role WHEN 'planner' THEN 0 ELSE 1 END LIMIT 1 FOR UPDATE`, ws, id).Scan(&j.ID, &j.Round, &j.Role, &j.Attempt, &input)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		if j.Attempt >= 2 {
			if e = goalFailRound(ctx, tx, ws, id, j.Round, "ATTEMPT_LIMIT"); e != nil {
				return e
			}
			return nil
		}
		var in goalInput
		if json.Unmarshal(input, &in) != nil {
			return problem(taskCodeStoreUnavailable)
		}
		j.WorkspaceID = ws
		j.RunID = id
		j.Window = in.Window
		j.Memory = in.Memory
		j.Goal = in.Goal
		j.Plan = in.Plan
		j.Attempt++
		if _, e = tx.Exec(ctx, "UPDATE core.goal_step SET state='RUNNING',attempt=$3,lease_until=clock_timestamp()+interval '30 seconds' WHERE workspace_id=$1 AND id=$2", ws, j.ID, j.Attempt); e != nil {
			return e
		}
		out = &j
		return nil
	})
	return out, e
}
func goalFailRound(ctx context.Context, tx pgx.Tx, ws, id string, n int, reason string) error {
	for _, q := range []string{"UPDATE core.goal_step SET state='FAILED' WHERE workspace_id=$1 AND run_id=$2 AND round=$3 AND state IN ('READY','BLOCKED','RUNNING')", "UPDATE core.goal_round SET state='FAILED' WHERE workspace_id=$1 AND run_id=$2 AND number=$3"} {
		if _, e := tx.Exec(ctx, q, ws, id, n); e != nil {
			return e
		}
	}
	_, e := tx.Exec(ctx, "UPDATE core.goal_run SET state='WAITING_INPUT',reason=$3,version=version+1 WHERE workspace_id=$1 AND id=$2", ws, id, reason+"；请提供有效窗口或人工补充")
	return e
}
func (g *GoalService) submit(ctx context.Context, j GoalJob, output json.RawMessage) error {
	if _, e := validateGoalOutput(j, output); e != nil {
		return e
	}
	d, _ := digest(json.RawMessage(output))
	var existing string
	e := g.store.Pool.QueryRow(ctx, `INSERT INTO core.goal_result(workspace_id,step_id,attempt,run_id,output,digest) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,step_id,attempt) DO UPDATE SET digest=core.goal_result.digest RETURNING digest`, j.WorkspaceID, j.ID, j.Attempt, j.RunID, []byte(output), d).Scan(&existing)
	if e == nil && existing != d {
		return problem(taskCodeIdempotency)
	}
	return e
}
func (g *GoalService) apply(ctx context.Context) error {
	return g.store.tx(ctx, func(tx pgx.Tx) error {
		var ws, id, step string
		var attempt int
		var raw []byte
		e := tx.QueryRow(ctx, `SELECT i.workspace_id,i.run_id,i.step_id,i.attempt,i.output FROM core.goal_result i JOIN core.goal_run r ON r.workspace_id=i.workspace_id AND r.id=i.run_id WHERE i.state='pending' ORDER BY i.run_id,i.step_id,i.attempt LIMIT 1 FOR UPDATE OF r SKIP LOCKED`).Scan(&ws, &id, &step, &attempt, &raw)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		r, e := goalRead(ctx, tx, ws, id, "", false)
		if e != nil {
			return e
		}
		var j GoalJob
		var state string
		var input []byte
		var lease time.Time
		e = tx.QueryRow(ctx, "SELECT round,role,attempt,state,input,lease_until FROM core.goal_step WHERE workspace_id=$1 AND id=$2 FOR UPDATE", ws, step).Scan(&j.Round, &j.Role, &j.Attempt, &state, &input, &lease)
		if e != nil {
			return e
		}
		inboxState := "stale"
		if r.State == "RUNNING" && r.Round == j.Round && state == "RUNNING" && attempt == j.Attempt && lease.After(time.Now()) && r.DeadlineAt.After(time.Now()) {
			var in goalInput
			if json.Unmarshal(input, &in) != nil {
				return problem(taskCodeStoreUnavailable)
			}
			j.ID = step
			j.RunID = id
			j.WorkspaceID = ws
			j.Window = in.Window
			j.Memory = in.Memory
			j.Goal = in.Goal
			j.Plan = in.Plan
			out, e := validateGoalOutput(j, raw)
			if e != nil {
				return e
			}
			if e = validateGoalWindow(j.Window, r.GroupID, time.Now()); e != nil {
				out.ErrorCode = Code(e)
			}
			if out.ErrorCode != "" {
				if e = goalFailRound(ctx, tx, ws, id, j.Round, out.ErrorCode); e != nil {
					return e
				}
			} else {
				if _, e = tx.Exec(ctx, "UPDATE core.goal_step SET state='COMPLETED',output=$3 WHERE workspace_id=$1 AND id=$2", ws, step, raw); e != nil {
					return e
				}
				if j.Role == "planner" {
					if _, e = tx.Exec(ctx, "UPDATE core.goal_round SET change_reason=$4,consumed_evidence_ids=$5 WHERE workspace_id=$1 AND run_id=$2 AND number=$3", ws, id, j.Round, out.ChangeReason, goalJSON(out.ConsumedEvidenceIDs)); e != nil {
						return e
					}
					intent := graphID("intent", ws, id, fmt.Sprint(j.Round))
					r.Graph.Nodes = append(r.Graph.Nodes, GoalNode{intent, "intent", out.ChangeReason, "planned"})
					r.Graph.Edges = append(r.Graph.Edges, GoalEdge{id, intent, "spawns"})
					r.Graph.Anchors = append(r.Graph.Anchors, GoalAnchor{intent, r.Graph.Assets[0].ID})
					for _, ref := range j.Memory.ConsumedEvidenceIDs {
						r.Graph.Edges = append(r.Graph.Edges, GoalEdge{intent, ref, "derived_from"})
					}
					if _, e = tx.Exec(ctx, "UPDATE core.goal_run SET graph=$3,version=version+1 WHERE workspace_id=$1 AND id=$2", ws, id, goalJSON(r.Graph)); e != nil {
						return e
					}
					in.Plan = &out
					if _, e = tx.Exec(ctx, "UPDATE core.goal_step SET state='READY',input=$4 WHERE workspace_id=$1 AND run_id=$2 AND round=$3 AND role='analyst' AND state='BLOCKED'", ws, id, j.Round, goalJSON(in)); e != nil {
						return e
					}
				} else {
					if e = g.finishAnalysis(ctx, tx, ws, &r, j, out); e != nil {
						return e
					}
				}
			}
			inboxState = "applied"
		}
		_, e = tx.Exec(ctx, "UPDATE core.goal_result SET state=$4 WHERE workspace_id=$1 AND step_id=$2 AND attempt=$3 AND state='pending'", ws, step, attempt, inboxState)
		return e
	})
}
func (g *GoalService) finishAnalysis(ctx context.Context, tx pgx.Tx, ws string, r *GoalRun, j GoalJob, out goalOutput) error {
	refs := goalRefs(j.Window)
	evidence := []GoalEvidence{}
	for i, m := range j.Window.Messages {
		evidence = append(evidence, GoalEvidence{refs[i], "raw", m.Text, []string{}, j.Round, j.Window.ExpiresAt})
	}
	evidence = append(evidence, GoalEvidence{j.ID + "_summary", "summary", out.Summary, refs, j.Round, j.Window.ExpiresAt}, GoalEvidence{j.ID + "_judgment", "model_judgment", "确定性测试判断：" + out.Summary, refs, j.Round, j.Window.ExpiresAt})
	for i, gap := range out.InformationGaps {
		evidence = append(evidence, GoalEvidence{fmt.Sprintf("%s_hint%d", j.ID, i), "hint", gap, refs, j.Round, j.Window.ExpiresAt})
	}
	intent := graphID("intent", ws, r.ID, fmt.Sprint(j.Round))
	for index := range r.Graph.Nodes {
		if r.Graph.Nodes[index].ID == intent {
			r.Graph.Nodes[index].State = "done"
		}
	}
	for _, v := range evidence {
		if _, e := tx.Exec(ctx, "INSERT INTO core.goal_evidence(workspace_id,run_id,id,round,kind,text,refs,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING", ws, r.ID, v.ID, v.Round, v.Kind, v.Text, goalJSON(v.Refs), v.ExpiresAt); e != nil {
			return e
		}
		kind := map[string]string{"raw": "fact", "summary": "digest", "model_judgment": "finding", "hint": "hint"}[v.Kind]
		r.Graph.Nodes = append(r.Graph.Nodes, GoalNode{v.ID, kind, v.Text, v.Kind})
		r.Graph.Edges = append(r.Graph.Edges, GoalEdge{intent, v.ID, "yields"})
		r.Graph.Anchors = append(r.Graph.Anchors, GoalAnchor{v.ID, r.Graph.Assets[0].ID})
		for _, ref := range v.Refs {
			r.Graph.Edges = append(r.Graph.Edges, GoalEdge{v.ID, ref, "derived_from"})
		}
	}
	state, reason := "WAITING_INPUT", "证据不足："+strings.Join(out.InformationGaps, "；")
	if out.SuggestedComplete {
		state = "AWAITING_CONFIRMATION"
		reason = "测试系统建议完成，等待操作员确认"
	} else if j.Round >= goalRounds {
		state = "LIMIT_REACHED"
		reason = "ROUND_LIMIT：证据仍不足，停止自动规划"
	}
	memory := GoalMemory{out.Summary, out.InformationGaps, refs, j.Round}
	if _, e := tx.Exec(ctx, "UPDATE core.goal_round SET state='COMPLETED' WHERE workspace_id=$1 AND run_id=$2 AND number=$3", ws, r.ID, j.Round); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, "UPDATE core.goal_run SET state=$3,reason=$4,memory=$5,graph=$6,version=version+1 WHERE workspace_id=$1 AND id=$2", ws, r.ID, state, reason, goalJSON(memory), goalJSON(r.Graph))
	return e
}

// Expiration includes waiting runs. All state transitions lock the same run row;
// no timer in memory is required to recover the deadline after restart.
func (g *GoalService) expire(ctx context.Context) error {
	return g.store.tx(ctx, func(tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT workspace_id,id FROM core.goal_run WHERE state IN ('WAITING_INPUT','RUNNING','AWAITING_CONFIRMATION') AND deadline_at<=clock_timestamp() ORDER BY deadline_at LIMIT 32 FOR UPDATE SKIP LOCKED`)
		if e != nil {
			return e
		}
		type identity struct{ ws, id string }
		ids := []identity{}
		for rows.Next() {
			var v identity
			if e = rows.Scan(&v.ws, &v.id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, v := range ids {
			if e = goalStopChildren(ctx, tx, v.ws, v.id); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE core.goal_run SET state='TIMED_OUT',reason='RUN_DEADLINE',version=version+1 WHERE workspace_id=$1 AND id=$2", v.ws, v.id); e != nil {
				return e
			}
		}
		return nil
	})
}
func (g *GoalService) Tick(ctx context.Context) error {
	if !g.fixture {
		return problem("GOAL_FIXTURE_DISABLED")
	}
	if e := g.expire(ctx); e != nil {
		return e
	}
	if e := g.publish(ctx); e != nil {
		return e
	}
	if e := g.apply(ctx); e != nil {
		return e
	}
	j, e := g.claim(ctx)
	if e != nil || j == nil {
		return e
	}
	callCtx, cancel := context.WithTimeout(ctx, goalAttemptTimeout)
	defer cancel()
	out, e := deterministicGoalModel(callCtx, *j)
	if e != nil {
		return e
	}
	return g.submit(ctx, *j, out)
}
func (g *GoalService) Run(ctx context.Context) error {
	if !g.fixture {
		return problem("GOAL_FIXTURE_DISABLED")
	}
	backoff := 200 * time.Millisecond
	for {
		// DB waits and the deterministic computation are bounded. Recovery is
		// always from the persisted outbox/inbox and the same logical steps.
		tickCtx, cancel := context.WithTimeout(ctx, goalAttemptTimeout+5*time.Second)
		e := g.Tick(tickCtx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if e != nil {
			if backoff < 5*time.Second {
				backoff *= 2
			}
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		} else {
			backoff = 200 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
