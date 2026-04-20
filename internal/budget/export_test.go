package budget

import (
	"context"
	"os/exec"
)

// SetExecCommand replaces execCommand for tests. Returns a restore func.
func SetExecCommand(f func(context.Context, string, ...string) *exec.Cmd) func() {
	orig := execCommand
	execCommand = f
	return func() { execCommand = orig }
}
