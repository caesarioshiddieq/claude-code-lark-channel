package intent_test

import (
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/intent"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		comment string
		want    intent.Phase
	}{
		// English implement verbs (positive — leading match).
		{"en_implement_lower", "implement OAuth flow", intent.PhaseImplement},
		{"en_implement_caps", "Implement the new feature", intent.PhaseImplement},
		{"en_implement_punct", "implement: add logout endpoint", intent.PhaseImplement},
		{"en_build", "build the API gateway", intent.PhaseImplement},
		{"en_build_caps", "Build a new component", intent.PhaseImplement},
		{"en_ship", "ship it now", intent.PhaseImplement},
		{"en_ship_caps", "Ship the migration script", intent.PhaseImplement},

		// Indonesian implement verbs (positive — leading match).
		{"id_kerjain", "kerjain fitur ini", intent.PhaseImplement},
		{"id_kerjain_caps", "Kerjain bug fix yang itu", intent.PhaseImplement},
		{"id_kerjain_punct", "kerjain, jangan lupa test", intent.PhaseImplement},
		{"id_buatin", "buatin endpoint baru", intent.PhaseImplement},
		{"id_buatin_caps", "Buatin OAuth setup dong", intent.PhaseImplement},

		// Negatives — must stay normal.
		{"empty", "", intent.PhaseNormal},
		{"whitespace_only", "   \t\n  ", intent.PhaseNormal},
		{"leading_can", "can you implement OAuth?", intent.PhaseNormal},
		{"leading_please_en", "please implement the search box", intent.PhaseNormal},
		{"leading_tolong_id", "tolong kerjain ini ya", intent.PhaseNormal},
		{"past_tense", "I implemented this yesterday", intent.PhaseNormal},
		{"different_word", "implementasi sudah selesai", intent.PhaseNormal},
		{"status_question", "what's the status?", intent.PhaseNormal},
		{"thanks", "thanks for shipping", intent.PhaseNormal},
		{"non_verb_punct", "@implement bot ping", intent.PhaseNormal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := intent.Classify(tt.comment)
			if got != tt.want {
				t.Errorf("Classify(%q) = %q, want %q", tt.comment, got, tt.want)
			}
		})
	}
}

func TestPhaseConstants(t *testing.T) {
	// Phase values must equal the SQL text values used in inbox.phase so
	// callers can write Classify(c) directly into the phase column without
	// translation.
	if string(intent.PhaseNormal) != "normal" {
		t.Errorf("PhaseNormal: got %q want %q", intent.PhaseNormal, "normal")
	}
	if string(intent.PhaseImplement) != "implement" {
		t.Errorf("PhaseImplement: got %q want %q", intent.PhaseImplement, "implement")
	}
}
