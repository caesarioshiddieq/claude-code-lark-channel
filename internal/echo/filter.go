package echo

import "github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"

// IsEchoComment returns true when creator.type == "app" (server-set, not spoofable).
// Echo comments are skipped to prevent infinite reply loops. Proven by Gate G5 smoke test.
func IsEchoComment(c lark.Comment) bool {
	return c.Creator.Type == "app"
}
