package sync

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Runner runs git with args in dir, with exactly the environment env,
// and returns what git printed to standard output. Tests use a fake one.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)
}

// waitDelay is how long Run waits for git's output to close after git
// was killed or has exited.
const waitDelay = 5 * time.Second

// maxOutput caps what Run keeps of git's output.
const maxOutput = 64 << 10

// execRunner runs a real program, git outside of tests.
//
// git starts helpers, such as git-remote-https for the download. They
// all run in a process group of their own, and when ctx ends, Run kills
// the whole group, so no helper keeps running after a timeout. This
// works on Unix only; Windows is not a target.
type execRunner struct {
	program string
}

func (r execRunner) Run(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.program, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// A negative ID sends the signal to the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	var stdout, stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("stopped: %w", ctx.Err())
		}
		if msg := lastLines(stderr.String(), 3); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// tailBuffer keeps the last maxOutput bytes written to it. The end of
// git's error output holds the reason it failed.
type tailBuffer struct {
	bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > maxOutput {
		p = p[len(p)-maxOutput:]
	}
	if over := b.Len() + len(p) - maxOutput; over > 0 {
		b.Next(over)
	}
	b.Buffer.Write(p)
	return n, nil
}

// lastLines returns the last n lines of s that are not blank, on one
// line.
func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " ")
}

// env returns the whole environment git runs with. Nothing comes from the
// server's own environment except PATH, so no setting there can change
// what git does.
//
// The token goes in only through GIT_CONFIG_COUNT, as an HTTP header for
// GitHub's address. It never appears in the URL, the arguments or
// .git/config.
func (s *Syncer) env(home, token string) []string {
	env := []string{
		"PATH=" + s.path,
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",       // no system git config
		"GIT_CONFIG_GLOBAL=/dev/null", // no user git config
		"GIT_TERMINAL_PROMPT=0",       // never wait for a password
		"GIT_ALLOW_PROTOCOL=https",    // no file://, ssh or ext:: transports
		"GIT_LFS_SKIP_SMUDGE=1",       // no large file downloads
	}
	if s.caFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+s.caFile)
	}
	if token != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+s.gitBase+"/.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basicAuth(token),
		)
	}
	return env
}

// basicAuth is the credential GitHub accepts from git for a token.
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// minGit is the oldest git the sync works with. 2.31 added
// GIT_CONFIG_COUNT, which carries the token, and 2.32 added
// GIT_CONFIG_GLOBAL, which keeps the user's git config out.
var minGit = [2]int{2, 32}

// CheckGit checks that git is installed and new enough. The server runs
// it at start, so a missing git stops it with a clear message instead of
// failing every sync.
func (s *Syncer) CheckGit(ctx context.Context) error {
	need := fmt.Sprintf("syncing repositories needs git %d.%d or newer", minGit[0], minGit[1])
	out, err := s.runner.Run(ctx, "", s.env(os.TempDir(), ""), "--version")
	if err != nil {
		return fmt.Errorf("%s: running git failed: %w", need, err)
	}
	major, minor, ok := parseGitVersion(string(out))
	if !ok {
		return fmt.Errorf("%s: cannot read the version in %q", need, strings.TrimSpace(string(out)))
	}
	if major < minGit[0] || major == minGit[0] && minor < minGit[1] {
		return fmt.Errorf("%s: found %d.%d", need, major, minor)
	}
	return nil
}

// parseGitVersion reads the major and minor version from the output of
// git --version, such as "git version 2.39.3 (Apple Git-145)".
func parseGitVersion(out string) (major, minor int, ok bool) {
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[0] != "git" || fields[1] != "version" {
		return 0, 0, false
	}
	parts := strings.Split(fields[2], ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}
