package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/issueguard"
)

// deliveryGateFixture creates a single issue (no parent needed — the gate
// check does not touch parent-notification logic). Cleanup removes the row
// and anything else the test inserted around it.
func newDeliveryGateFixture(t *testing.T, status string) IssueResponse {
	t.Helper()

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "delivery-gate issue " + time.Now().Format(time.RFC3339Nano),
		"status": status,
	})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create issue: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&issue); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issue.ID)
	})
	return issue
}

// attachCodeLabelViaAPI creates (if needed) the workspace's "code" issue
// label and attaches it to the given issue through the real AttachLabel
// handler (not raw SQL), since the label-attach path is what the escape
// hatch tests below also exercise for its audit-comment side effect.
func attachLabelViaAPI(t *testing.T, issueID, labelName string) {
	t.Helper()

	var labelID string
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue_label (workspace_id, resource_type, name, color)
		VALUES ($1, 'issue', $2, '#000000')
		ON CONFLICT DO NOTHING
		RETURNING id`, testWorkspaceID, labelName).Scan(&labelID)
	if err != nil {
		lookupErr := testPool.QueryRow(context.Background(), `
			SELECT id FROM issue_label
			WHERE workspace_id = $1 AND resource_type = 'issue' AND LOWER(name) = LOWER($2)`,
			testWorkspaceID, labelName).Scan(&labelID)
		if lookupErr != nil {
			t.Fatalf("create/lookup %s label: insert=%v lookup=%v", labelName, err, lookupErr)
		}
	}

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues/"+issueID+"/labels", map[string]any{"label_id": labelID})
	req = withURLParam(req, "id", issueID)
	testHandler.AttachLabel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("AttachLabel(%s): expected 200, got %d: %s", labelName, w.Code, w.Body.String())
	}
}

func attachCodeLabel(t *testing.T, issueID string) {
	t.Helper()
	attachLabelViaAPI(t, issueID, issueguard.DeliveryGateLabel)
}

// linkPullRequest inserts a minimal github_pull_request + issue_pull_request
// pair so the issue satisfies IsDeliveryGated criterion 2 (and, when merged
// with closeIntent, GetIssuePullRequestCloseAggregate's positive case).
func linkPullRequest(t *testing.T, issueID string, state string, closeIntent bool) {
	t.Helper()
	ctx := context.Background()

	var prID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO github_pull_request (
			workspace_id, installation_id, repo_owner, repo_name, pr_number,
			title, state, html_url, pr_created_at, pr_updated_at, head_sha,
			merged_at
		) VALUES (
			$1, 1, 'FlexmediaIS', 'test-repo', floor(random() * 1000000)::int,
			'test pr', $2, 'https://github.com/FlexmediaIS/test-repo/pull/1', now(), now(), 'deadbeef',
			CASE WHEN $2 = 'merged' THEN now() ELSE NULL END
		) RETURNING id`, testWorkspaceID, state).Scan(&prID)
	if err != nil {
		t.Fatalf("insert github_pull_request: %v", err)
	}

	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue_pull_request (issue_id, pull_request_id, close_intent, reference_only)
		VALUES ($1, $2, $3, false)`, issueID, prID, closeIntent,
	); err != nil {
		t.Fatalf("insert issue_pull_request: %v", err)
	}

	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id = $1`, prID)
	})
}

// captureLogs redirects the package-default slog output to an in-memory
// buffer for the duration of the test, mirroring the pattern in
// skill_test.go. Returns the buffer to inspect after the exercised call.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})
	return &logs
}

// updateIssueStatusAsAgent drives UpdateIssue with the X-Agent-ID/X-Task-ID
// header pair resolveActor requires to trust an agent actor (see
// agent_test.go / agent_access_test.go for the established pattern).
func updateIssueStatusAsAgent(t *testing.T, issueID, status, agentID, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": status})
	req = withURLParam(req, "id", issueID)
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	testHandler.UpdateIssue(w, req)
	return w
}

func containsUndeliveredWarning(log string) bool {
	return bytes.Contains([]byte(log), []byte(issueguard.UndeliveredDoneMessage))
}

// TestDeliveryGate_UngatedIssueClosesSilently — an issue with no "code"
// label, no linked PR, and a member (non-agent) assignee must never warn
// or reject, regardless of actor. This is the "zero workflow change"
// guarantee for research/docs/client-question cards (done-gate-scope.md
// "hard part").
func TestDeliveryGate_UngatedIssueClosesSilently(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	logs := captureLogs(t)

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "done"})
	req = withURLParam(req, "id", issue.ID)
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("expected no delivery-gate warning for an ungated issue, got log:\n%s", got)
	}
}

// TestDeliveryGate_MemberActorNeverRejected — the plan's key asymmetry:
// only agent callers are gated. A member closing a labeled "code" issue
// with no PR must succeed even though the issue itself is gated.
func TestDeliveryGate_MemberActorNeverRejected(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	logs := captureLogs(t)

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "done"})
	req = withURLParam(req, "id", issue.ID)
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200 for a member actor, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("expected no delivery-gate warning for a member actor, got log:\n%s", got)
	}
}

// TestDeliveryGate_LabeledIssueRejectedWithoutMergedPR — criterion 1
// (label "code"). An agent closing it with no linked PR at all must be
// rejected with a 400 (Phase 3 step 2: reject mode), and the status must
// NOT have changed.
func TestDeliveryGate_LabeledIssueRejectedWithoutMergedPR(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-1", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UpdateIssue: expected 400 (reject mode), got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning for a labeled issue with no PR, got log:\n%s", got)
	}

	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, issue.ID).Scan(&status); err != nil {
		t.Fatalf("read issue status: %v", err)
	}
	if status != "in_progress" {
		t.Fatalf("expected status unchanged at in_progress after rejection, got %q", status)
	}
}

// TestDeliveryGate_LinkedIssueRejectedWithoutCloseIntent — criterion 2 (has
// a PR row). A merged PR that merely mentions the issue (no close intent)
// must still be rejected — the plan is explicit that a link without close
// intent does not satisfy the gate.
func TestDeliveryGate_LinkedIssueRejectedWithoutCloseIntent(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	linkPullRequest(t, issue.ID, "merged", false)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-2", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UpdateIssue: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning for merged-without-close-intent, got log:\n%s", got)
	}
}

// TestDeliveryGate_MergedWithCloseIntentSucceeds — the satisfied case: a
// merged PR that declared explicit closing intent must succeed, for either
// criterion 1 or 2.
func TestDeliveryGate_MergedWithCloseIntentSucceeds(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	linkPullRequest(t, issue.ID, "merged", true)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-3", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200 once a merged PR declared close intent, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("expected no delivery-gate warning once a merged PR declared close intent, got log:\n%s", got)
	}
}

// TestDeliveryGate_NoCodeDeliveryLabelExemptsFromRejection is the escape
// hatch's core guarantee: applying no-code-delivery to an otherwise-gated
// (labeled "code", no PR) issue must let the agent close it successfully.
func TestDeliveryGate_NoCodeDeliveryLabelExemptsFromRejection(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	attachLabelViaAPI(t, issue.ID, issueguard.NoCodeDeliveryLabel)

	agentID := createHandlerTestAgent(t, "delivery-gate-agent-4", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, the no-code-delivery label must exempt this issue even though it also carries \"code\", got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("no-code-delivery label should fully exempt the issue, got log:\n%s", got)
	}
}

// TestDeliveryGate_NoCodeDeliveryLabelAlonePostsAuditComment verifies the
// escape hatch is visible, not silent (done-gate-scope.md "Escape hatch"):
// attaching the label must post a system comment naming the actor, and
// that comment must NOT be a triggering mention://agent/ or
// mention://member/ link (which would wake the referenced actor as a side
// effect of applying the exemption — see label.go's AttachLabel).
func TestDeliveryGate_NoCodeDeliveryLabelAlonePostsAuditComment(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")

	before := countSystemCommentsOn(t, issue.ID)
	attachLabelViaAPI(t, issue.ID, issueguard.NoCodeDeliveryLabel)
	after := countSystemCommentsOn(t, issue.ID)

	if after != before+1 {
		t.Fatalf("expected exactly one new system comment from the escape-hatch label attach, before=%d after=%d", before, after)
	}

	content, _, _, _ := systemCommentOn(t, issue.ID)
	if !bytes.Contains([]byte(content), []byte(issueguard.NoCodeDeliveryLabel)) {
		t.Fatalf("expected the audit comment to name the %s label, got: %s", issueguard.NoCodeDeliveryLabel, content)
	}
	if bytes.Contains([]byte(content), []byte("mention://agent/")) || bytes.Contains([]byte(content), []byte("mention://member/")) {
		t.Fatalf("escape-hatch audit comment must not be a triggering mention, got: %s", content)
	}
}

// TestDeliveryGate_CodeLabelAloneDoesNotPostAuditComment guards against a
// future refactor accidentally posting the escape-hatch comment for every
// label attach instead of only no-code-delivery.
func TestDeliveryGate_CodeLabelAloneDoesNotPostAuditComment(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")

	before := countSystemCommentsOn(t, issue.ID)
	attachCodeLabel(t, issue.ID)
	after := countSystemCommentsOn(t, issue.ID)

	if after != before {
		t.Fatalf("attaching the \"code\" label should not post an escape-hatch comment, before=%d after=%d", before, after)
	}
}

// TestDeliveryGate_CodeWritingAgentAssigneeRejected — criterion 3
// (assignee is a code-writing agent). No label, no PR link, but the issue
// is assigned to an agent whose name matches
// issueguard.CodeWritingAgentNames — the actor closing it (not necessarily
// the assignee) is still rejected.
func TestDeliveryGate_CodeWritingAgentAssigneeRejected(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")

	assigneeAgentID := createHandlerTestAgent(t, "Backend Dev", nil)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET assignee_type = 'agent', assignee_id = $1 WHERE id = $2`,
		assigneeAgentID, issue.ID,
	); err != nil {
		t.Fatalf("assign issue to code-writing agent: %v", err)
	}

	actingAgentID := createHandlerTestAgent(t, "delivery-gate-agent-5", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, actingAgentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", actingAgentID, taskID)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("UpdateIssue: expected 400, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning when assignee is a code-writing agent, got log:\n%s", got)
	}
}

// TestDeliveryGate_BatchUpdateSkipsRejectedIssueButProcessesRest verifies
// BatchUpdateIssues does not bypass the gate (an agent batch-closing
// several issues at once must not slip a gated one through), and that a
// rejected issue does not abort the rest of the batch — consistent with
// every other per-issue failure mode already in that loop.
func TestDeliveryGate_BatchUpdateSkipsRejectedIssueButProcessesRest(t *testing.T) {
	gated := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, gated.ID)
	ungated := newDeliveryGateFixture(t, "in_progress")

	agentID := createHandlerTestAgent(t, "delivery-gate-agent-6", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, gated.ID)

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{gated.ID, ungated.ID},
		"updates":   map[string]any{"status": "done"},
	})
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	testHandler.BatchUpdateIssues(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("BatchUpdateIssues: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var gatedStatus, ungatedStatus string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, gated.ID).Scan(&gatedStatus); err != nil {
		t.Fatalf("read gated issue status: %v", err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, ungated.ID).Scan(&ungatedStatus); err != nil {
		t.Fatalf("read ungated issue status: %v", err)
	}
	if gatedStatus != "in_progress" {
		t.Fatalf("expected gated issue to be skipped (status unchanged), got %q", gatedStatus)
	}
	if ungatedStatus != "done" {
		t.Fatalf("expected ungated issue to still be closed by the same batch, got %q", ungatedStatus)
	}
}
