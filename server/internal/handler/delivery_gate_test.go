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

// attachCodeLabel creates (if needed) the workspace's "code" issue label and
// attaches it to the given issue.
func attachCodeLabel(t *testing.T, issueID string) {
	t.Helper()

	var labelID string
	err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue_label (workspace_id, resource_type, name, color)
		VALUES ($1, 'issue', 'code', '#000000')
		ON CONFLICT DO NOTHING
		RETURNING id`, testWorkspaceID).Scan(&labelID)
	if err != nil {
		// ON CONFLICT DO NOTHING with no matching unique index still returns
		// a row on success; on true conflict there's no explicit constraint
		// here, so look the existing label up instead of assuming one race.
		lookupErr := testPool.QueryRow(context.Background(), `
			SELECT id FROM issue_label
			WHERE workspace_id = $1 AND resource_type = 'issue' AND LOWER(name) = 'code'`,
			testWorkspaceID).Scan(&labelID)
		if lookupErr != nil {
			t.Fatalf("create/lookup code label: insert=%v lookup=%v", err, lookupErr)
		}
	}

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		issueID, labelID,
	); err != nil {
		t.Fatalf("attach code label: %v", err)
	}
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

// TestDeliveryGate_UngatedIssueClosesSilently — an issue with no "code"
// label, no linked PR, and a member (non-agent) assignee must never warn,
// regardless of actor. This is the "zero workflow change" guarantee for
// research/docs/client-question cards (done-gate-scope.md "hard part").
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

// TestDeliveryGate_MemberActorNeverWarns — the plan's key asymmetry: only
// agent callers are gated. A member closing a labeled "code" issue with no
// PR must not trigger the warning even though the issue itself is gated.
func TestDeliveryGate_MemberActorNeverWarns(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	logs := captureLogs(t)

	w := httptest.NewRecorder()
	req := newRequest("PUT", "/api/issues/"+issue.ID, map[string]any{"status": "done"})
	req = withURLParam(req, "id", issue.ID)
	testHandler.UpdateIssue(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("expected no delivery-gate warning for a member actor, got log:\n%s", got)
	}
}

// TestDeliveryGate_LabeledIssueWarnsWithoutMergedPR — criterion 1 (label
// "code"). An agent closing it with no linked PR at all must warn.
func TestDeliveryGate_LabeledIssueWarnsWithoutMergedPR(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-1", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200 (log-only, never rejects), got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning for a labeled issue with no PR, got log:\n%s", got)
	}
}

// TestDeliveryGate_LinkedIssueWarnsWithoutCloseIntent — criterion 2 (has a
// PR row). A merged PR that merely mentions the issue (no close intent)
// must still warn — the plan is explicit that a link without close intent
// does not satisfy the gate.
func TestDeliveryGate_LinkedIssueWarnsWithoutCloseIntent(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	linkPullRequest(t, issue.ID, "merged", false)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-2", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning for merged-without-close-intent, got log:\n%s", got)
	}
}

// TestDeliveryGate_MergedWithCloseIntentNeverWarns — the satisfied case:
// a merged PR that declared explicit closing intent must never warn, for
// either criterion 1 or 2.
func TestDeliveryGate_MergedWithCloseIntentNeverWarns(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")
	attachCodeLabel(t, issue.ID)
	linkPullRequest(t, issue.ID, "merged", true)
	agentID := createHandlerTestAgent(t, "delivery-gate-agent-3", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("expected no delivery-gate warning once a merged PR declared close intent, got log:\n%s", got)
	}
}

// TestDeliveryGate_NoCodeDeliveryLabelIsNotSelfExempting documents current
// behavior: IsDeliveryGated only implements the three gating criteria, not
// the "no-code-delivery" escape hatch. Applying the escape-hatch label
// alone does not add a "code" label, so an otherwise-ungated issue stays
// ungated — this guards against a future refactor accidentally treating
// the exemption label itself as a gating label.
func TestDeliveryGate_NoCodeDeliveryLabelIsNotSelfExempting(t *testing.T) {
	issue := newDeliveryGateFixture(t, "in_progress")

	var labelID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue_label (workspace_id, resource_type, name, color)
		VALUES ($1, 'issue', $2, '#111111')
		RETURNING id`, testWorkspaceID, issueguard.NoCodeDeliveryLabel).Scan(&labelID); err != nil {
		t.Fatalf("create no-code-delivery label: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, issue.ID, labelID,
	); err != nil {
		t.Fatalf("attach no-code-delivery label: %v", err)
	}

	agentID := createHandlerTestAgent(t, "delivery-gate-agent-4", nil)
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issue.ID)
	logs := captureLogs(t)

	w := updateIssueStatusAsAgent(t, issue.ID, "done", agentID, taskID)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); containsUndeliveredWarning(got) {
		t.Fatalf("no-code-delivery label alone should not gate the issue, got log:\n%s", got)
	}
}

// TestDeliveryGate_CodeWritingAgentAssigneeWarns — criterion 3 (assignee is
// a code-writing agent). No label, no PR link, but the issue is assigned to
// an agent whose name matches issueguard.CodeWritingAgentNames — the actor
// closing it (not necessarily the assignee) is still gated.
func TestDeliveryGate_CodeWritingAgentAssigneeWarns(t *testing.T) {
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
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateIssue: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if got := logs.String(); !containsUndeliveredWarning(got) {
		t.Fatalf("expected delivery-gate warning when assignee is a code-writing agent, got log:\n%s", got)
	}
}

func containsUndeliveredWarning(log string) bool {
	return bytes.Contains([]byte(log), []byte(issueguard.UndeliveredDoneMessage))
}
