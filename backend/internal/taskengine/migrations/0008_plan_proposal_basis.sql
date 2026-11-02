-- Proposal basis: provenance for one generated proposal. It records which live
-- Summary version and which graph anchor the generation actually consulted, and
-- the rule that selected them, so a second observation can be shown to have
-- changed the next proposal's input. The Summary stays reference data: the BBA
-- ledger (executions, results, claims, permits) remains execution truth, and this
-- table grants no authority by itself. One append-only row per created proposal.
SET lock_timeout = '5s';

CREATE TABLE core.plan_proposal_basis(
 workspace_id core.id NOT NULL,
 proposal_id core.id NOT NULL,
 task_id core.id NOT NULL,
 account_id core.id NOT NULL,
 group_id core.qq_id NOT NULL,
 target_id core.id NOT NULL,
 anchor_id core.id,
 summary_id text,
 summary_version integer,
 evidence_kinds jsonb,
 source_manifest_sha256 text,
 selection_reason text NOT NULL CHECK(length(selection_reason) BETWEEN 1 AND 500),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(workspace_id,proposal_id),
 FOREIGN KEY(workspace_id,proposal_id) REFERENCES core.plan_proposal(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id) REFERENCES core.task(workspace_id,id),
 FOREIGN KEY(workspace_id,task_id,account_id) REFERENCES core.task_account(workspace_id,task_id,account_id),
 FOREIGN KEY(workspace_id,task_id,target_id) REFERENCES core.task_target(workspace_id,task_id,target_id),
 FOREIGN KEY(workspace_id,task_id,anchor_id) REFERENCES core.task_graph_anchor(workspace_id,task_id,anchor_id),
 FOREIGN KEY(workspace_id,account_id,group_id,summary_id) REFERENCES core.group_summary(workspace_id,account_id,group_id,summary_id),
 CHECK((summary_id IS NULL AND summary_version IS NULL AND evidence_kinds IS NULL AND source_manifest_sha256 IS NULL) OR
       (summary_id ~ '^[a-f0-9]{64}$' AND summary_version>0 AND jsonb_typeof(evidence_kinds)='array' AND source_manifest_sha256 ~ '^[a-f0-9]{64}$')),
 CHECK(evidence_kinds IS NULL OR jsonb_array_length(evidence_kinds)<=20)
);
CREATE INDEX plan_proposal_basis_task ON core.plan_proposal_basis(workspace_id,task_id,created_at);
CREATE TRIGGER plan_proposal_basis_append_only BEFORE UPDATE OR DELETE ON core.plan_proposal_basis FOR EACH ROW EXECUTE FUNCTION core.append_only();
INSERT INTO core.schema_version(version) VALUES(17);
