// Package intent maps Lark task comment text to a routing phase used by the
// supervisor's worker dispatch fork. The MVP heuristic is leading-verb match
// — a comment whose first word (lowercased, trailing punctuation stripped) is
// one of the verbs in [implement, build, ship, kerjain, buatin] classifies as
// PhaseImplement; everything else stays PhaseNormal.
//
// The heuristic is deliberately conservative. False-negatives — a missed
// implement-intent gets answered by claude -p, which the user can re-trigger
// — are preferred over false-positives, which would burn a per-window token
// budget on the wrong task. Hard-mode classification (full natural-language
// understanding) is out of scope for the MVP and deferred to a future PRD;
// see docs/superpowers/plans/2026-04-27-autonomous-implementer-plan.md
// "Risks accepted (MVP)" row "Wrong intent classifier" for the rationale.
//
// Bilingual support (EN+ID) reflects the project's existing language
// preference; new verbs should be added to implementVerbs only after a
// false-positive review window confirms no collision with common
// non-implement leading words in either language.
package intent

import "strings"

// Phase is the routing phase assigned to an inbox row. Values match the SQL
// text values used in inbox.phase (see internal/sqlite/queue.go) so callers
// can store Classify output directly into the column without translation.
type Phase string

const (
	PhaseNormal    Phase = "normal"
	PhaseImplement Phase = "implement"
)

// implementVerbs is the leading-word allowlist that triggers PhaseImplement.
// EN: implement, build, ship. ID: kerjain, buatin.
var implementVerbs = []string{"implement", "build", "ship", "kerjain", "buatin"}

// trailingPunct is stripped from the leading word before verb match so
// "implement!" and "kerjain," classify the same as the bare verb.
const trailingPunct = ".,!?:;-"

// Classify returns PhaseImplement when comment's first word (lowercased,
// trailing punctuation stripped) is one of implementVerbs. Otherwise
// PhaseNormal. Empty / whitespace-only comments classify as PhaseNormal.
func Classify(comment string) Phase {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(comment)))
	if len(fields) == 0 {
		return PhaseNormal
	}
	leading := strings.TrimRight(fields[0], trailingPunct)
	for _, v := range implementVerbs {
		if leading == v {
			return PhaseImplement
		}
	}
	return PhaseNormal
}
