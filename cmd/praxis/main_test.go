package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The exit codes are the command's contract with anything that automates it,
// so they are tested by running the built binary rather than by calling
// functions that return them.
func buildPraxis(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "praxis")
	out, err := exec.Command("go", "build", "-o", binary, "praxis/cmd/praxis").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return binary
}

func runPraxis(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run: %v", err)
	}
	return string(out), exit.ExitCode()
}

func TestExitCodes(t *testing.T) {
	binary := buildPraxis(t)
	dir := t.TempDir()

	t.Run("a path that does not exist", func(t *testing.T) {
		_, code := runPraxis(t, binary, "store", "inspect", filepath.Join(dir, "absent"))
		if code != exitNoInput {
			t.Fatalf("exit: got %d, want %d", code, exitNoInput)
		}
	})

	t.Run("a file that is not a journal", func(t *testing.T) {
		path := filepath.Join(dir, "not-a-journal")
		if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, code := runPraxis(t, binary, "store", "inspect", path)
		if code != exitFatal {
			t.Fatalf("exit: got %d, want %d", code, exitFatal)
		}
	})

	t.Run("an empty file exists and is corrupt, not missing", func(t *testing.T) {
		path := filepath.Join(dir, "empty")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, code := runPraxis(t, binary, "store", "inspect", path)
		if code != exitFatal {
			t.Fatalf("exit: got %d, want %d", code, exitFatal)
		}
	})

	t.Run("no arguments", func(t *testing.T) {
		out, code := runPraxis(t, binary)
		if code != exitFatal || !strings.Contains(out, "usage:") {
			t.Fatalf("exit %d, output %q", code, out)
		}
	})

	t.Run("an unknown subcommand", func(t *testing.T) {
		_, code := runPraxis(t, binary, "store", "vacuum", "x")
		if code != exitFatal {
			t.Fatalf("exit: got %d, want %d", code, exitFatal)
		}
	})
}

// Scenario: a clean journal exits 0, a repairable one exits 1 until it is
// repaired, and then exits 0
//
// A successful repair must not exit non-zero: a shell running under `set -e`
// would read a correct repair as a failure.
func TestRepairLifecycleExitCodes(t *testing.T) {
	binary := buildPraxis(t)
	path := filepath.Join(t.TempDir(), "journal.praxis")

	writeJournalForCLI(t, path)

	out, code := runPraxis(t, binary, "store", "inspect", path)
	if code != exitClean {
		t.Fatalf("a clean journal: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "condition: clean") {
		t.Fatalf("report does not say it is clean:\n%s", out)
	}

	truncateJournalForCLI(t, path, 10)

	out, code = runPraxis(t, binary, "store", "inspect", path)
	if code != exitRepairable {
		t.Fatalf("a damaged journal: exit %d\n%s", code, out)
	}

	out, code = runPraxis(t, binary, "store", "repair", path)
	if code != exitRepairable || !strings.Contains(out, "nothing was changed") {
		t.Fatalf("a dry run: exit %d\n%s", code, out)
	}

	out, code = runPraxis(t, binary, "store", "repair", path, "--apply")
	if code != exitClean {
		t.Fatalf("an applied repair: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "repaired:") || !strings.Contains(out, "evidence kept at") {
		t.Fatalf("the repair does not report what it did:\n%s", out)
	}

	_, code = runPraxis(t, binary, "store", "inspect", path)
	if code != exitClean {
		t.Fatalf("after repair: exit %d", code)
	}
}
