package execenv

import (
	"strings"
	"testing"
)

// Regression: the Repositories section must precede the agent-identity block,
// which inlines unbounded AgentInstructions. A provider that injects only a
// prefix of the brief would otherwise truncate the checkout imperative away.
func TestRepositoriesPrecedesAgentInstructions(t *testing.T) {
	ctx := TaskContextForEnv{
		AgentName:         "DevOps",
		AgentID:           "agent-1",
		IssueID:           "issue-1",
		AgentInstructions: strings.Repeat("PADDING RULE 6x. prove delivery.\n", 2000),
		Repos: []RepoContextForEnv{
			{URL: "https://github.com/FlexmediaIS/ips.git"},
		},
	}
	out := buildMetaSkillContentSlim("codex", ctx)

	repos := strings.Index(out, "## Repositories")
	instr := strings.Index(out, "PADDING RULE 6x")
	if repos < 0 {
		t.Fatal("Repositories section missing")
	}
	if instr < 0 {
		t.Fatal("agent instructions missing")
	}
	if repos > instr {
		t.Fatalf("Repositories at %d is AFTER agent instructions at %d", repos, instr)
	}
	if repos > 8000 {
		t.Errorf("Repositories at char %d; too deep to survive prefix truncation", repos)
	}
	if n := strings.Count(out, "## Repositories"); n != 1 {
		t.Errorf("Repositories emitted %d times, want 1", n)
	}
}
