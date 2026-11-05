package taskengine

// This deliberately deterministic test implementation is NOT a real model.
// Its independently validated output contract cannot express arbitrary tools,
// request_join, Task creation or QQ sends. Future real-model adapters must use
// this contract, the existing registry/provider transport, and separate consent.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type goalOutput struct {
	SchemaID            string   `json:"schema_id"`
	Role                string   `json:"role"`
	ConsumedEvidenceIDs []string `json:"consumed_evidence_ids"`
	Intent              string   `json:"intent"`
	ChangeReason        string   `json:"change_reason"`
	Summary             string   `json:"summary"`
	InformationGaps     []string `json:"information_gaps"`
	SuggestedComplete   bool     `json:"suggested_complete"`
	ErrorCode           string   `json:"error_code"`
}

func goalRefs(w GoalWindow) []string {
	refs := make([]string, 0, len(w.Messages))
	for _, m := range w.Messages {
		refs = append(refs, graphID("synthetic", w.GroupID, w.EventID, m.ID))
	}
	return refs
}
func deterministicGoalModel(ctx context.Context, j GoalJob) (json.RawMessage, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	out := goalOutput{SchemaID: "GoalPlan.v1", Role: j.Role, ConsumedEvidenceIDs: goalRefs(j.Window), InformationGaps: []string{}}
	if j.Role == "planner" {
		out.Intent = "analyze_window"
		focus := "首轮建立需求、预算与时间的证据检查"
		if j.Memory.UpdatedRound > 0 {
			focus = "复核上一轮缺口：" + strings.Join(j.Memory.InformationGaps, "、")
		}
		out.ChangeReason = fmt.Sprintf("%s；消费%d条新合成证据；上一轮%d：%s；本轮仅分析指定群，不扩大范围", focus, len(j.Window.Messages), j.Memory.UpdatedRound, j.Memory.Summary)
	} else {
		if j.Plan == nil || j.Plan.Intent != "analyze_window" {
			return nil, problem("GOAL_PLAN_REQUIRED")
		}
		planner := j
		planner.Role = "planner"
		if _, e := validateGoalOutput(planner, goalJSON(j.Plan)); e != nil {
			return nil, e
		}
		out.SchemaID = "GoalAnalysis.v1"
		text := ""
		for _, m := range j.Window.Messages {
			text += m.Text + "\n"
		}
		for _, field := range []string{"需求：", "预算：", "时间："} {
			if !strings.Contains(text, field) {
				out.InformationGaps = append(out.InformationGaps, "请补充"+strings.TrimSuffix(field, "："))
			}
		}
		out.SuggestedComplete = len(out.InformationGaps) == 0
		out.Summary = fmt.Sprintf("仅按测试字段规则检查%d条合成消息，发现%d项信息缺口", len(j.Window.Messages), len(out.InformationGaps))
	}
	return goalJSON(out), nil
}
func validateGoalOutput(j GoalJob, raw json.RawMessage) (goalOutput, error) {
	var out goalOutput
	if len(raw) > 16384 {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 9 {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	for _, field := range fields {
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return out, problem("GOAL_OUTPUT_INVALID")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil || dec.Decode(new(any)) != io.EOF {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	schema := "GoalPlan.v1"
	if j.Role == "analyst" {
		schema = "GoalAnalysis.v1"
	}
	if out.SchemaID != schema || out.Role != j.Role || len(out.Summary) > 2000 || len(out.ChangeReason) > 4000 || len(out.InformationGaps) > 8 {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	refs := goalRefs(j.Window)
	if len(refs) != len(out.ConsumedEvidenceIDs) {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	for i, ref := range refs {
		if out.ConsumedEvidenceIDs[i] != ref {
			return out, problem("GOAL_OUTPUT_INVALID")
		}
	}
	if out.ErrorCode != "" && out.ErrorCode != "WINDOW_EXPIRED" && out.ErrorCode != "ANALYSIS_FAILED" {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	if j.Role == "planner" && (out.Intent != "analyze_window" || out.ChangeReason == "" || out.SuggestedComplete || out.Summary != "" || len(out.InformationGaps) != 0) {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	if j.Role == "analyst" && (out.Intent != "" || out.ChangeReason != "" || out.Summary == "" || out.SuggestedComplete != (len(out.InformationGaps) == 0)) {
		return out, problem("GOAL_OUTPUT_INVALID")
	}
	for _, gap := range out.InformationGaps {
		if strings.TrimSpace(gap) == "" || len(gap) > 500 {
			return out, problem("GOAL_OUTPUT_INVALID")
		}
	}
	return out, nil
}
