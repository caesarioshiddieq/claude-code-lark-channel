package echo_test

import (
	"testing"

	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/echo"
	"github.com/caesarioshiddieq/claude-code-lark-channel/internal/lark"
)

func TestIsEchoComment(t *testing.T) {
	cases := []struct {
		name    string
		comment lark.Comment
		want    bool
	}{
		{"bot is echo", lark.Comment{Creator: lark.Creator{ID: "cli_abc", Type: "app"}}, true},
		{"user not echo", lark.Comment{Creator: lark.Creator{ID: "u1", Type: "user"}}, false},
		{"empty type not echo", lark.Comment{Creator: lark.Creator{Type: ""}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := echo.IsEchoComment(tc.comment); got != tc.want {
				t.Errorf("IsEchoComment(%+v) = %v, want %v", tc.comment, got, tc.want)
			}
		})
	}
}
