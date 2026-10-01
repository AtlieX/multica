package issueguard

import "testing"

func TestIsIntakeAgentName(t *testing.T) {
	for name, want := range map[string]bool{
		"Client Intake":   true,
		" client intake ": true,
		"Tech Lead":       false,
		"Frontend Design": false,
		"":                false,
	} {
		if got := IsIntakeAgentName(name); got != want {
			t.Errorf("IsIntakeAgentName(%q) = %v, want %v", name, got, want)
		}
	}
}
