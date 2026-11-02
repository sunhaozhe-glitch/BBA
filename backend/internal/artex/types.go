package artex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Node kinds as ARTEX stores them in exploration_nodes.kind: ARTEX/db/constants.go:5-11.
const (
	KindGoal    = "goal"
	KindIntent  = "intent"
	KindFact    = "fact"
	KindFinding = "finding"
	KindHint    = "hint"
	KindDigest  = "digest"
)

// Edge relations as ARTEX stores them in exploration_edges.rel: ARTEX/db/constants.go:34-39.
const (
	RelSpawns      = "spawns"
	RelDerivedFrom = "derived_from"
	RelYields      = "yields"
	RelProves      = "proves"
	RelCovers      = "covers"
)

// Node is one ARTEX exploration node (a fact, intent, goal, …) mapped from the
// wire shape of ARTEX/server/dto.go:183-211.
type Node struct {
	// ID is ARTEX's decimal node id, transported as a JSON string
	// (ARTEX/server/dto.go:18) and parsed here; ids are positive.
	ID int64
	// Kind is the wire field "type" (ARTEX's node kind, e.g. KindFact).
	Kind string
	// State is the ARTEX node state. Fact states are confirmed/dismissed/origin
	// (ARTEX/db/schema.sql:189); the adapter never remaps them.
	State string
	// Priority is the ARTEX scheduling priority, carried through unchanged.
	Priority int
	// Origin is the producer label ("worker", "planner", …); ARTEX always emits
	// it (empty string when the column is NULL): ARTEX/db/exploration.go:718.
	Origin string
	// Payload is the raw payload JSON. On the wire it is a JSON *string* holding
	// JSON text (ARTEX/server/dto.go:24). Nil when ARTEX emitted JSON null.
	Payload json.RawMessage
	// TS is ARTEX's "ts" field, i.e. exploration_nodes.created_at as RFC3339
	// (ARTEX/server/dto.go:20,196-211), normalized to UTC.
	TS time.Time
	// SourceTaskID is set only for nodes inherited from a directly related task
	// (wire field omitted otherwise): ARTEX/server/dto.go:207-210.
	SourceTaskID int64
	// Inherited mirrors the wire "inherited" flag for such source-task nodes.
	Inherited bool
	// DeleteReason is the operator-supplied reason for a soft-deleted intent;
	// empty otherwise: ARTEX/server/dto.go:191,199.
	DeleteReason string
}

// Edge is one exploration edge: ARTEX/server/dto.go:268-277.
type Edge struct {
	Src int64
	Dst int64
	Rel string
}

// Graph is the whole readable exploration graph of a task:
// ARTEX/server/server.go:2630-2674.
type Graph struct {
	Nodes []Node
	Edges []Edge
}

// NodePage is one offset page of exploration nodes plus the edges touching the
// page and the neighbour nodes those edges point at:
// ARTEX/server/server.go:2676-2741 (envelope at 2732-2740).
type NodePage struct {
	Items  []Node
	Total  int
	Page   int
	Size   int
	Edges  []Edge
	Refs   map[int64]Node
	Assets json.RawMessage // unconsumed: ARTEX asset map keyed by node id
}

// ---- wire shapes (field-for-field copies of the ARTEX DTOs) ----

type nodeWire struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Payload      string `json:"payload"`
	Priority     int    `json:"priority"`
	State        string `json:"state"`
	Origin       string `json:"origin"`
	TS           string `json:"ts"`
	SourceTaskID string `json:"source_task_id"`
	Inherited    bool   `json:"inherited"`
	DeleteReason string `json:"delete_reason"`
}

type edgeWire struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	Rel string `json:"rel"`
}

type graphWire struct {
	Nodes []nodeWire `json:"nodes"`
	Edges []edgeWire `json:"edges"`
}

type nodesPageWire struct {
	Items  []nodeWire          `json:"items"`
	Total  int                 `json:"total"`
	Page   int                 `json:"page"`
	Size   int                 `json:"size"`
	Edges  []edgeWire          `json:"edges"`
	Refs   map[string]nodeWire `json:"refs"`
	Assets json.RawMessage     `json:"assets"`
}

// ---- wire → domain conversion with strict validation ----

func parseWireID(field, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s is missing", field)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a decimal integer", field, raw)
	}
	if id <= 0 {
		return 0, fmt.Errorf("%s %d is not positive", field, id)
	}
	return id, nil
}

func (w nodeWire) toNode() (Node, error) {
	id, err := parseWireID("node id", w.ID)
	if err != nil {
		return Node{}, err
	}
	if strings.TrimSpace(w.Type) == "" {
		return Node{}, fmt.Errorf("node %d has no type", id)
	}
	if strings.TrimSpace(w.State) == "" {
		return Node{}, fmt.Errorf("node %d has no state", id)
	}
	if strings.TrimSpace(w.TS) == "" {
		return Node{}, fmt.Errorf("node %d has no ts", id)
	}
	ts, err := time.Parse(time.RFC3339, w.TS)
	if err != nil {
		return Node{}, fmt.Errorf("node %d ts %q is not RFC3339", id, w.TS)
	}
	n := Node{
		ID:           id,
		Kind:         w.Type,
		State:        w.State,
		Priority:     w.Priority,
		Origin:       w.Origin,
		TS:           ts.UTC(),
		Inherited:    w.Inherited,
		DeleteReason: w.DeleteReason,
	}
	// ARTEX serializes the payload as JSON text inside a JSON string; the empty
	// string only happens for a zero-length payload, and "null" for an explicit
	// JSON null. Both mean "no payload" to a consumer.
	if payload := strings.TrimSpace(w.Payload); payload != "" && payload != "null" {
		if !json.Valid([]byte(payload)) {
			return Node{}, fmt.Errorf("node %d payload is not valid JSON", id)
		}
		n.Payload = json.RawMessage(payload)
	}
	if w.SourceTaskID != "" {
		sid, err := parseWireID("source_task_id", w.SourceTaskID)
		if err != nil {
			return Node{}, fmt.Errorf("node %d: %w", id, err)
		}
		n.SourceTaskID = sid
	}
	return n, nil
}

func (w edgeWire) toEdge() (Edge, error) {
	src, err := parseWireID("edge src", w.Src)
	if err != nil {
		return Edge{}, err
	}
	dst, err := parseWireID("edge dst", w.Dst)
	if err != nil {
		return Edge{}, err
	}
	if strings.TrimSpace(w.Rel) == "" {
		return Edge{}, fmt.Errorf("edge %d->%d has no rel", src, dst)
	}
	return Edge{Src: src, Dst: dst, Rel: w.Rel}, nil
}

func toNodes(in []nodeWire) ([]Node, error) {
	out := make([]Node, 0, len(in))
	for _, w := range in {
		n, err := w.toNode()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func toEdges(in []edgeWire) ([]Edge, error) {
	out := make([]Edge, 0, len(in))
	for _, w := range in {
		e, err := w.toEdge()
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (w graphWire) toGraph() (Graph, error) {
	nodes, err := toNodes(w.Nodes)
	if err != nil {
		return Graph{}, err
	}
	edges, err := toEdges(w.Edges)
	if err != nil {
		return Graph{}, err
	}
	return Graph{Nodes: nodes, Edges: edges}, nil
}

func (w nodesPageWire) toPage() (NodePage, error) {
	items, err := toNodes(w.Items)
	if err != nil {
		return NodePage{}, err
	}
	edges, err := toEdges(w.Edges)
	if err != nil {
		return NodePage{}, err
	}
	var refs map[int64]Node
	if len(w.Refs) > 0 {
		refs = make(map[int64]Node, len(w.Refs))
		for key, wire := range w.Refs {
			id, err := parseWireID("refs key", key)
			if err != nil {
				return NodePage{}, err
			}
			node, err := wire.toNode()
			if err != nil {
				return NodePage{}, err
			}
			if node.ID != id {
				return NodePage{}, fmt.Errorf("refs key %d carries node %d", id, node.ID)
			}
			refs[id] = node
		}
	}
	return NodePage{
		Items:  items,
		Total:  w.Total,
		Page:   w.Page,
		Size:   w.Size,
		Edges:  edges,
		Refs:   refs,
		Assets: w.Assets,
	}, nil
}

// errNoNode reports that a requested node id is absent from the filtered page.
var errNoNode = errors.New("node id is not present in the ARTEX response")
