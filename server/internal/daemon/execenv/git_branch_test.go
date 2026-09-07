package execenv

import (
	"strings"
	"testing"
)

func TestTaskKeyUsesTail(t *testing.T) {
	t.Parallel()

	const id = "01a01ec0-e69d-7000-8000-0123456789ab"
	if got := taskKey(id); got != "0123456789ab" {
		t.Fatalf("taskKey(%q) = %q, want 0123456789ab", id, got)
	}
	if got := TaskKey(id); got != "0123456789ab" {
		t.Fatalf("TaskKey(%q) = %q, want 0123456789ab", id, got)
	}
}

func TestIssueBranchSegmentUsesIssueIdentifier(t *testing.T) {
	t.Parallel()

	const taskID = "01a01ec0-e69d-7000-8000-0123456789ab"
	got := IssueBranchSegment("MUL-6063", taskID)
	if !strings.Contains(got, "mul-6063") {
		t.Fatalf("IssueBranchSegment did not include the issue identifier: %q", got)
	}
	if !strings.HasSuffix(got, taskKey(taskID)) {
		t.Fatalf("IssueBranchSegment did not preserve the task suffix: %q", got)
	}
	if fallback := IssueBranchSegment("", taskID); fallback != taskKey(taskID) {
		t.Fatalf("IssueBranchSegment fallback = %q, want %q", fallback, taskKey(taskID))
	}
}
