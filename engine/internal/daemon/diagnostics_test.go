package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestBootLogTailBoundsBytesLinesAndLaunchOffset(t *testing.T) {
	path := t.TempDir() + "/console.log"
	old := "old boot failure\n"
	current := strings.Repeat("boot output\n", 2000) + "/init: mkdir: not found\nKernel panic\n"
	if err := os.WriteFile(path, []byte(old+current), 0600); err != nil {
		t.Fatal(err)
	}
	tail := (bootLog{path: path, offset: int64(len(old)), lines: 60}).tail()
	if len(tail) > diagnosticBytes || len(strings.Split(tail, "\n")) > 60 ||
		!strings.Contains(tail, "mkdir: not found") || !strings.Contains(tail, "Kernel panic") {
		t.Fatalf("missing or unbounded diagnostic tail: %q", tail)
	}
	if tail := (bootLog{path: path, offset: int64(len(old + current)), lines: 60}).tail(); tail != "" {
		t.Fatalf("old console output reported for silent new launch: %q", tail)
	}
	if err := os.WriteFile(path, []byte(old+"/init failed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if tail := (bootLog{path: path, offset: int64(len(old)), lines: 60}).tail(); tail != "/init failed" {
		t.Fatalf("launch boundary not preserved: %q", tail)
	}
}

func TestBootDiagnosticsHandleMissingLogsAndPreserveCause(t *testing.T) {
	path := t.TempDir() + "/missing.log"
	cause := errors.New("VMM exited")
	process := &child{logs: []bootLog{{path: path, lines: 60}}}
	err := process.diagnosticError(cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), path) {
		t.Fatalf("diagnostics obscured original failure: %v", err)
	}
	target := t.TempDir() + "/other.log"
	if err := os.WriteFile(target, []byte("unrelated private file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(process.diagnosticError(cause).Error(), "unrelated private file") {
		t.Fatal("diagnostics followed a replaced log symlink")
	}
}
