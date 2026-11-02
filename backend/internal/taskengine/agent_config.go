package taskengine

import (
	"encoding/json"
	"strings"
	"time"
)

type SkillBinding struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}
type ConfigMeta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Version     int    `json:"version"`
	UpdatedBy   string `json:"updated_by"`
	UpdatedAt   string `json:"updated_at"`
	Deleted     bool   `json:"deleted,omitempty"`
}
type SkillProfile struct {
	ConfigMeta
	Goal                 string   `json:"goal"`
	Inputs               []string `json:"inputs"`
	Steps                []string `json:"steps"`
	RequiredCapabilities []string `json:"required_capabilities"`
	Outputs              []string `json:"outputs"`
	StopConditions       []string `json:"stop_conditions"`
	Constraints          []string `json:"constraints"`
	Tools                []string `json:"tools"`
	Published            bool     `json:"published"`
}
type AgentProfile struct {
	WebSearch bool `json:"web_search"`
	ConfigMeta
	SystemPrompt string         `json:"system_prompt"`
	ModelID      string         `json:"model_id"`
	Skills       []SkillBinding `json:"skills"`
	Tools        []string       `json:"tools"`
}
type AgentReceipt struct {
	Search *SearchReceipt `json:"search,omitempty"`
	Agent  AgentProfile   `json:"agent"`
	Skills []SkillProfile `json:"skills"`
	Model  ModelProfile   `json:"model"`
}

func configKey(workspace, id string) string { return workspace + "/" + id }
func (r *ModelRegistry) cloneConfig() modelRegistryDisk {
	b, _ := json.Marshal(r.data)
	var d modelRegistryDisk
	_ = json.Unmarshal(b, &d)
	if d.Agents == nil {
		d.Agents = map[string][]AgentProfile{}
	}
	if d.Skills == nil {
		d.Skills = map[string][]SkillProfile{}
	}
	if d.AgentHistory == nil {
		d.AgentHistory = map[string][]AgentProfile{}
	}
	if d.SkillHistory == nil {
		d.SkillHistory = map[string][]SkillProfile{}
	}
	if d.Receipts == nil {
		d.Receipts = map[string]AgentReceipt{}
	}
	return d
}
func validConfigMeta(m ConfigMeta) bool {
	return strings.TrimSpace(m.Name) != "" && len(m.Name) <= 100 && len(m.Description) <= 4000
}
func validLines(xs []string) bool {
	if len(xs) > 80 {
		return false
	}
	for _, x := range xs {
		if strings.TrimSpace(x) == "" || len(x) > 4000 {
			return false
		}
	}
	return true
}
func (r *ModelRegistry) modelAvailable(ws, id string) bool {
	for _, m := range r.data.Workspaces[ws] {
		if m.ID == id {
			return m.Enabled && m.APIKey != ""
		}
	}
	return false
}
func (r *ModelRegistry) skillSnapshot(ws string, b SkillBinding) (SkillProfile, error) {
	live := false
	for _, s := range r.data.Skills[ws] {
		if s.ID == b.ID {
			live = s.Enabled
		}
	}
	if !live {
		return SkillProfile{}, problem("SKILL_REFERENCE_UNAVAILABLE")
	}
	for _, s := range r.data.SkillHistory[configKey(ws, b.ID)] {
		if s.Version == b.Version && !s.Deleted && s.Enabled && s.Published {
			return s, nil
		}
	}
	return SkillProfile{}, problem("SKILL_VERSION_UNAVAILABLE")
}
func (r *ModelRegistry) saveSkill(ws, actor, id string, s SkillProfile, expected int, remove bool) (SkillProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.cloneConfig()
	items := d.Skills[ws]
	idx := -1
	old := SkillProfile{}
	for i, x := range items {
		if x.ID == id {
			idx = i
			old = x
		}
	}
	if id != "" && idx < 0 {
		return s, problem(taskCodeNotFound)
	}
	if expected != old.Version {
		return s, problem(taskCodeVersionConflict)
	}
	if remove || !s.Enabled {
		for _, a := range d.Agents[ws] {
			for _, b := range a.Skills {
				if b.ID == id {
					return s, problem("CONFIG_REFERENCED")
				}
			}
		}
	}
	if !remove {
		if !validConfigMeta(s.ConfigMeta) || strings.TrimSpace(s.Goal) == "" || len(s.Goal) > 8000 || len(s.Steps) == 0 || len(s.StopConditions) == 0 || !validLines(s.Inputs) || !validLines(s.Steps) || !validLines(s.Outputs) || !validLines(s.Constraints) || !validLines(s.StopConditions) || !validLines(s.RequiredCapabilities) || len(s.Tools) > 0 {
			return s, problem(taskCodeValidation)
		}
		for _, c := range s.RequiredCapabilities {
			if c != "structured_output" {
				return s, problem("CAPABILITY_UNVERIFIED")
			}
		}
		if s.Enabled && !s.Published {
			return s, problem("SKILL_UNPUBLISHED")
		}
	} else {
		s = old
	}
	if id == "" {
		if len(items) >= 100 {
			return s, problem(taskCodeStateConflict)
		}
		id = newID("skill")
	}
	s.ConfigMeta = ConfigMeta{ID: id, Name: s.Name, Description: s.Description, Enabled: s.Enabled, Version: expected + 1, UpdatedBy: actor, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Deleted: remove}
	if s.Inputs == nil {
		s.Inputs = []string{}
	}
	if s.Outputs == nil {
		s.Outputs = []string{}
	}
	if s.Constraints == nil {
		s.Constraints = []string{}
	}
	if s.RequiredCapabilities == nil {
		s.RequiredCapabilities = []string{}
	}
	s.Tools = []string{}
	if remove {
		items = append(items[:idx], items[idx+1:]...)
	} else if idx < 0 {
		items = append(items, s)
	} else {
		items[idx] = s
	}
	d.Skills[ws] = items
	k := configKey(ws, id)
	d.SkillHistory[k] = append(d.SkillHistory[k], s)
	if r.persist(d) != nil {
		return SkillProfile{}, problem(taskCodeStoreUnavailable)
	}
	r.data = d
	return s, nil
}
func (r *ModelRegistry) saveAgent(ws, actor, id string, a AgentProfile, expected int, remove bool) (AgentProfile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.cloneConfig()
	items := d.Agents[ws]
	idx := -1
	old := AgentProfile{}
	for i, x := range items {
		if x.ID == id {
			idx = i
			old = x
		}
	}
	if id != "" && idx < 0 {
		return a, problem(taskCodeNotFound)
	}
	if expected != old.Version {
		return a, problem(taskCodeVersionConflict)
	}
	if !remove {
		if !validConfigMeta(a.ConfigMeta) || len(a.SystemPrompt) > 16000 || len(a.Tools) > 0 || len(a.Skills) > 20 {
			return a, problem(taskCodeValidation)
		}
		if !r.modelAvailable(ws, a.ModelID) {
			return a, problem("MODEL_REFERENCE_UNAVAILABLE")
		}
		seen := map[string]bool{}
		for _, b := range a.Skills {
			if seen[b.ID] {
				return a, problem(taskCodeValidation)
			}
			seen[b.ID] = true
			if _, err := r.skillSnapshot(ws, b); err != nil {
				return a, err
			}
		}
	} else {
		a = old
	}
	if id == "" {
		if len(items) >= 100 {
			return a, problem(taskCodeStateConflict)
		}
		id = newID("agent")
	}
	a.ConfigMeta = ConfigMeta{ID: id, Name: a.Name, Description: a.Description, Enabled: a.Enabled, Version: expected + 1, UpdatedBy: actor, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Deleted: remove}
	if a.Skills == nil {
		a.Skills = []SkillBinding{}
	}
	a.Tools = []string{}
	if remove {
		items = append(items[:idx], items[idx+1:]...)
	} else if idx < 0 {
		items = append(items, a)
	} else {
		items[idx] = a
	}
	d.Agents[ws] = items
	k := configKey(ws, id)
	d.AgentHistory[k] = append(d.AgentHistory[k], a)
	if r.persist(d) != nil {
		return AgentProfile{}, problem(taskCodeStoreUnavailable)
	}
	r.data = d
	return a, nil
}
