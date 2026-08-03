package issueguard

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// DeliveryGateLabel is the issue label that opts an issue into the
// delivery gate (done-gate-scope.md Phase 2, criterion 1).
const DeliveryGateLabel = "code"

// NoCodeDeliveryLabel is the escape hatch: applying it to a delivery-gated
// issue exempts it, and the exemption must be recorded as a system comment
// naming who applied it (done-gate-scope.md "Escape hatch"). Checking for
// this label is the caller's responsibility — IsDeliveryGated only reports
// whether an issue would otherwise be gated.
const NoCodeDeliveryLabel = "no-code-delivery"

// CodeWritingAgentNames are agent names treated as code-writing for
// criterion 3 of the delivery gate. There is no dedicated "role" column on
// agent (only Kind, which distinguishes system/user agents), so this is a
// case-insensitive name match. Keep in sync with done-gate-scope.md.
var CodeWritingAgentNames = []string{
	"backend dev",
	"frontend design",
	"devops",
}

func isCodeWritingAgentName(name string) bool {
	lower := strings.ToLower(name)
	for _, candidate := range CodeWritingAgentNames {
		if lower == candidate {
			return true
		}
	}
	return false
}

// HasIssueLabel reports whether the issue carries an issue-scoped label
// with the given name (case-insensitive), workspace-guarded the same way
// as ListLabelsByIssue.
func HasIssueLabel(ctx context.Context, db_ dbExecutor, issueID, workspaceID pgtype.UUID, labelName string) (bool, error) {
	var exists bool
	err := db_.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM issue_label l
			JOIN issue_to_label il ON il.label_id = l.id
			WHERE il.issue_id = $1
			  AND l.workspace_id = $2
			  AND l.resource_type = 'issue'
			  AND LOWER(l.name) = LOWER($3)
		)`, issueID, workspaceID, labelName).Scan(&exists)
	return exists, err
}

// HasLinkedPullRequest reports whether any issue_pull_request row references
// this issue (criterion 2 — a PR referenced it, so it is code work). Unlike
// GetIssuePullRequestCloseAggregate this intentionally does NOT filter out
// reference_only rows: even a body-only mention means a PR exists for this
// issue, which is enough to treat it as code work for gating purposes.
func HasLinkedPullRequest(ctx context.Context, db_ dbExecutor, issueID pgtype.UUID) (bool, error) {
	var exists bool
	err := db_.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM issue_pull_request WHERE issue_id = $1
		)`, issueID).Scan(&exists)
	return exists, err
}

// dbExecutor mirrors handler.dbExecutor so this package does not import
// internal/handler (which already imports issueguard).
type dbExecutor interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// IsDeliveryGated implements done-gate-scope.md's three criteria for an
// issue being delivery-gated: label "code", an existing PR link, or an
// assignee that is a code-writing agent. Non-agent assignees (member,
// squad, or unassigned) never satisfy criterion 3.
func IsDeliveryGated(ctx context.Context, db_ dbExecutor, q *db.Queries, issue db.Issue) (bool, error) {
	hasLabel, err := HasIssueLabel(ctx, db_, issue.ID, issue.WorkspaceID, DeliveryGateLabel)
	if err != nil {
		return false, err
	}
	if hasLabel {
		return true, nil
	}

	hasPR, err := HasLinkedPullRequest(ctx, db_, issue.ID)
	if err != nil {
		return false, err
	}
	if hasPR {
		return true, nil
	}

	if issue.AssigneeType.String == "agent" && issue.AssigneeID.Valid {
		agent, err := q.GetAgent(ctx, issue.AssigneeID)
		if err != nil {
			if err == pgx.ErrNoRows {
				return false, nil
			}
			return false, err
		}
		if isCodeWritingAgentName(agent.Name) {
			return true, nil
		}
	}

	return false, nil
}

// UndeliveredDoneMessage is the log message emitted (Phase 3: log-only,
// not rejected) when an agent closes a delivery-gated issue with no merged
// PR carrying explicit close intent.
const UndeliveredDoneMessage = "agent closed a delivery-gated code issue with no merged PR carrying close intent"
