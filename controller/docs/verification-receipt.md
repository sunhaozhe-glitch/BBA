# Phase 1 verification receipt

Scope: independent development Controller, ISOLATED_FIXTURE only. No real OMP/model invocation, QQ/production operation, service deployment or merge.

Baseline: 43c049c42d66f51bd1b59750d2750bccb8d46b66. Branch: feat/controller-phase1.

Command, from repository root:

    PYTHONPATH=controller/src python3 -m unittest discover -s controller/tests -q

Parent execution after ROUND2: 59 tests in 84.381s, OK, exit 0. Tests include actual MCP SDK stdio clients, simulated ACP subprocesses, isolated Git repositories/worktrees, SQLite concurrency/crash recovery and model-policy simulations.

Independent ROUND2 reviewer: ship, high confidence, no findings; R1-1/R1-2/R1-3 closed within fixture scope. Reviewer independently ran 59 tests in 97.496s, OK, exit 0; 18 boundary observations and their cleanup checks passed; four copied-code mutations were caught by behavioral assertions. This review is separate from GitHub approval and CI.

The sustained-audit-failure reproducer ran three times after correction of its old defect assertion: both peers exited, owner closed, two unresolved slots retained, and the injected storage error remained explicit. The parent's unmodified historical reproducer observed the same repaired behavior but exited 1 because it still asserted the old defect. That exit is not reported as a passing test.

Historical raw output, RED/GREEN logs, probe copies and private manifests remain local under controller/docs/evidence/, excluded by .gitignore. Public reports reference those local artifacts; they are not included in this checkout after cloning. This receipt is a summary, not a replacement for raw evidence. No product-code changes were made after independent ROUND2 review; subsequent preparation only excludes private evidence and adds this receipt.

Known limits: real model identity/routing, OMP lifecycle and cancellation, network/serving quotas, monetary/token billing, actual disk failures and production are not verified. Simulated budgets use simulation-credit. Safety events keep a bounded recent window; unresolved outcomes retain budgets/slots and require reconciliation. Default OBSERVE and real-model denial remain enforced.

GitHub PR and latest-head CI are reported separately after remote reads; this file does not claim either has run.
