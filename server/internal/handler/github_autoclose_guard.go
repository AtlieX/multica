package handler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/issueguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// closingTailRe matches the text right after a closing-keyword match when it
// ends the close declaration. "Fixes SPA-1", "Closes SPA-1.", "(fixes SPA-1)"
// and "Fixes MUL-2 resolves MUL-3" all qualify; "Fixes: SPA-63 (made on /vorur)
// could not be found" does not, because there the identifier is the subject of
// a sentence, not the object of the keyword. A literal two-character "\n" is
// accepted because some agents write PR bodies with escaped newlines.
var closingTailRe = regexp.MustCompile(
	`^[ \t]*(?:$|\r?\n|\\n|[.,;:!)\]]|(?i:and|close[sd]?|fix(?:e[sd])?|resolve[sd]?)\b)`,
)

// autoCloseWithheldForIntake reports whether a merged PR's close intent must
// not advance this issue because it is still with the Client Intake agent.
// Client Intake never writes code and hands work to Tech Lead before anything
// is built, so a PR that "closes" an intake-held card is a mis-reference
// (SPA-63, 2026-10-01). The check is on the assignee rather than on
// metadata.waiting_on, which is free text and routinely left stale.
func (h *Handler) autoCloseWithheldForIntake(ctx context.Context, issue db.Issue) bool {
	if !issue.AssigneeID.Valid || issue.AssigneeType.String != "agent" {
		return false
	}
	agent, err := h.Queries.GetAgent(ctx, issue.AssigneeID)
	if err != nil {
		return false
	}
	return issueguard.IsIntakeAgentName(agent.Name)
}

// postAutoCloseWithheldComment leaves a system comment on an issue whose
// auto-close was withheld, so the skipped transition is visible rather than
// silent. System comments do not trigger the assignee, so Client Intake is not
// woken up and nothing is relayed to the client. Deduplicated per PR so a
// webhook redelivery does not stack comments.
func (h *Handler) postAutoCloseWithheldComment(ctx context.Context, issue db.Issue, p *ghPullRequestPayload) {
	ref := fmt.Sprintf("%s/%s#%d", p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number)
	marker := "<!-- autoclose-withheld:" + ref + " -->"

	var exists bool
	if err := h.DB.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM comment WHERE issue_id = $1 AND content LIKE '%' || $2 || '%')`,
		issue.ID, marker,
	).Scan(&exists); err != nil || exists {
		return
	}

	content := fmt.Sprintf(
		"[%s](%s) merged and declared a closing keyword for this issue, but it was not moved to done because the issue is still with Client Intake. If the PR really delivers this request, close it by hand.\n%s",
		ref, p.PullRequest.HTMLURL, marker,
	)
	created, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("github: autoclose-withheld comment failed", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(created.Comment(), nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      created.IssueRevision,
	})
}
