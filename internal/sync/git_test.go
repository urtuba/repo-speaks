package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunner(t *testing.T) {
	r := execRunner{program: "sh"}
	ctx := context.Background()
	env := []string{"PATH=" + os.Getenv("PATH"), "GREETING=hello"}

	out, err := r.Run(ctx, t.TempDir(), env, "-c", "echo $GREETING; echo $HOME")
	if err != nil {
		t.Fatal(err)
	}
	// Only the environment given reaches the program: HOME is not set.
	if got := string(out); got != "hello\n\n" {
		t.Errorf("output = %q, want only what the environment holds", got)
	}

	_, err = r.Run(ctx, "", env, "-c", "echo out; echo first >&2; echo fatal: no access >&2; exit 3")
	if err == nil || err.Error() != "exit status 3: first fatal: no access" {
		t.Errorf("error = %v, want the exit status and the error output", err)
	}
}

// TestExecRunnerKillsProcessGroup checks that a timeout kills the program
// and the processes it started, as git starts git-remote-https.
func TestExecRunnerKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := execRunner{program: "sh"}.Run(ctx, "", []string{"PATH=" + os.Getenv("PATH")},
		"-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidFile)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Run returned after %v, want soon after the timeout", d)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	// The child is killed with the group. It may stay a zombie for a
	// moment, until the system reaps it.
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the child process outlived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTailBuffer(t *testing.T) {
	var b tailBuffer
	b.Write([]byte(strings.Repeat("a", maxOutput)))
	b.Write([]byte("end"))
	if b.Len() != maxOutput || !strings.HasSuffix(b.String(), "aend") {
		t.Errorf("len = %d, want %d ending with the last write", b.Len(), maxOutput)
	}
	b.Write([]byte(strings.Repeat("b", maxOutput+10)))
	if b.String() != strings.Repeat("b", maxOutput) {
		t.Error("a write over the cap must keep its own end")
	}
}

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		out          string
		major, minor int
		ok           bool
	}{
		{"git version 2.39.3 (Apple Git-145)\n", 2, 39, true},
		{"git version 2.32.0", 2, 32, true},
		{"git version 2.45.1.windows.1", 2, 45, true},
		{"git version 2", 0, 0, false},
		{"git version x.y", 0, 0, false},
		{"version 2.39.3", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		major, minor, ok := parseGitVersion(tt.out)
		if major != tt.major || minor != tt.minor || ok != tt.ok {
			t.Errorf("parseGitVersion(%q) = %d, %d, %v, want %d, %d, %v", tt.out, major, minor, ok, tt.major, tt.minor, tt.ok)
		}
	}
}
