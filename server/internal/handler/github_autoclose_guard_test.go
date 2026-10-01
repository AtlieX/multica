package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestWebhook_MergedPR_DoesNotCloseIntakeHeldIssue is the SPA-63 repro: a
// merged PR declaring "Fixes X" must not move X to done while X is still held
// by Client Intake. A control issue held by another agent still closes, and a
// redelivered webhook does not stack a second withheld comment.
func TestWebhook_MergedPR_DoesNotCloseIntakeHeldIssue(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	secret := "intake-guard-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	seedAgent := func(name string) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent (workspace_id, name, description, runtime_mode, runtime_config,
				runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id,
				instructions, custom_env, custom_args)
			VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 'public_to', 1, $4,
				'', '{}'::jsonb, '[]'::jsonb)
			RETURNING id`, testWorkspaceID, name, handlerTestRuntimeID(t), testUserID).Scan(&id); err != nil {
			t.Fatalf("seed agent %q: %v", name, err)
		}
		return id
	}
	createIssue := func(title, agentID string) IssueResponse {
		t.Helper()
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
			"title":  title,
			"status": "in_progress",
		})
		w := testutil.Call(t, testHandler.CreateIssue, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue %q: %d %s", title, w.Code, w.Body.String())
		}
		var out IssueResponse
		json.NewDecoder(w.Body).Decode(&out)
		// Assign directly so no task is dispatched to the seeded agent.
		if _, err := testPool.Exec(ctx,
			`UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`,
			out.ID, agentID); err != nil {
			t.Fatalf("assign %q: %v", title, err)
		}
		return out
	}

	intakeID := seedAgent("Client Intake")
	techLeadID := seedAgent("Tech Lead")
	held := createIssue("client request awaiting answers", intakeID)
	control := createIssue("real work", techLeadID)

	t.Cleanup(func() {
		c := context.Background()
		for _, id := range []string{held.ID, control.ID} {
			testPool.Exec(c, `DELETE FROM comment WHERE issue_id = $1`, id)
			testPool.Exec(c, `DELETE FROM issue_pull_request WHERE issue_id = $1`, id)
			testPool.Exec(c, `DELETE FROM activity_log WHERE issue_id = $1`, id)
			testPool.Exec(c, `DELETE FROM issue WHERE id = $1`, id)
		}
		testPool.Exec(c, `DELETE FROM agent WHERE id = ANY($1::uuid[])`, []string{intakeID, techLeadID})
		testPool.Exec(c, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(c, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID)
	})

	const installationID int64 = 30264063
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "intake-guard-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	fireBareWebhook(t, secret, installationID, 63, "feat: widget lists every page", "Fixes "+held.Identifier+"\n", "feat/widget")
	fireBareWebhook(t, secret, installationID, 63, "feat: widget lists every page", "Fixes "+held.Identifier+"\n", "feat/widget")
	fireBareWebhook(t, secret, installationID, 64, "fix: real work", "Fixes "+control.Identifier+"\n", "fix/real")

	for id, want := range map[string]string{held.ID: "in_progress", control.ID: "done"} {
		got, err := testHandler.Queries.GetIssue(ctx, parseUUID(id))
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		if got.Status != want {
			t.Errorf("issue %s: status = %q, want %q", id, got.Status, want)
		}
	}

	var withheld int
	testPool.QueryRow(ctx, `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%autoclose-withheld:acme/widget#63%'`,
		held.ID).Scan(&withheld)
	if withheld != 1 {
		t.Errorf("withheld comments on intake-held issue = %d, want 1", withheld)
	}
}
