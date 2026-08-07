// Minimal stand-in for internal/testenv, for this isolated mutation-testing
// fixture. Only the handful of functions crypto/internal/cryptotest actually
// calls are implemented, with straightforward (non-skip) behavior -- this
// fixture always has optimization on, network access, and a working `go`
// tool in PATH, so the real package's skip-guards would all be no-ops here
// anyway.
package testenv

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func SkipIfOptimizationOff(t testing.TB) {}

func MustHaveExternalNetwork(t testing.TB) {}

func CleanCmdEnv(cmd *exec.Cmd) *exec.Cmd { return cmd }

func Command(t testing.TB, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(context.Background(), name, args...)
}

func GoToolPath(t testing.TB) string {
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func Executable(t testing.TB) string {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func Builder() string { return os.Getenv("GO_BUILDER_NAME") }
