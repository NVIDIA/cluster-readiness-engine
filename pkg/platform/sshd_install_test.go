// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sshdInstallCommand tests run the command under /bin/sh against fake
// apt-get and dpkg, so they exercise what the text-only invariants cannot:
// that a hung attempt is killed and leaves nothing behind, that an interrupted
// install is recovered before the retry, and that the attempts run out. They
// need the host's GNU timeout (and one needs util-linux setsid): without them
// they skip locally and fail in CI, where they are the only coverage of the
// command's runtime behaviour.
//
// The fakes log every call to $CALLS and keep state in $STATE. The command's
// sshd path is moved into the test directory, and its timeouts are shortened
// to ten seconds: short enough to keep the hang cases quick, long enough that a
// fake never times out under a loaded test run.

// sshdInstallRun is the outcome of one run of sshdInstallCommand.
type sshdInstallRun struct {
	exitCode int
	stderr   string
	calls    []string
	dir      string
}

// count returns how many logged calls start with prefix.
func (r sshdInstallRun) count(prefix string) int {
	n := 0
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const (
	fakeCallUpdate    = "apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Dpkg::Use-Pty=0 update"
	fakeCallInstall   = "apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Dpkg::Use-Pty=0 install"
	fakeCallConfigure = "dpkg --configure -a"
)

// fakeDpkg finishes a half-done install, as dpkg --configure -a does.
const fakeDpkg = `#!/bin/sh
echo "dpkg $*" >> "$CALLS"
/bin/rm -f "$STATE/half-installed"
`

// requireHostTool returns the path of a host tool these tests need. Without it
// they skip locally but fail in CI, the same split requireHelm makes in
// test/helm: a runner that lost the tool would otherwise skip the only tests
// that execute sshdInstallCommand, and stay green.
func requireHostTool(t *testing.T, name, what string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is required in CI but %s is not on PATH", what, name)
		}
		t.Skipf("no %s on PATH; these tests need %s", name, what)
	}
	return path
}

// processGone reports whether pid has exited. A killed process whose parent
// died with it is reparented, and stays a zombie until something reaps it:
// briefly under a real init, forever in a container whose PID 1 is go test
// itself. kill(pid, 0) still succeeds on a zombie, so on Linux the state in
// /proc counts a zombie as gone; elsewhere only ESRCH does.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name, which may itself
	// contain spaces or parentheses.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

// runSSHDInstall writes the fakes into a fresh directory and runs the command.
// withTimeout puts the host's timeout on PATH; prebaked creates the sshd the
// command checks for.
func runSSHDInstall(t *testing.T, fakeAptGet string, withTimeout, prebaked bool) sshdInstallRun {
	t.Helper()
	timeoutPath := requireHostTool(t, "timeout", "GNU coreutils timeout")

	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755)) //nolint:gosec // test fakes must be executable
	}
	write("apt-get", fakeAptGet)
	write("dpkg", fakeDpkg)
	if prebaked {
		write("sshd", "#!/bin/sh\n")
	}

	script := sshdInstallCommand
	for old, replacement := range map[string]string{
		"/usr/sbin/sshd":      filepath.Join(dir, "sshd"),
		"timeout -s KILL 120": "timeout -s KILL 10",
	} {
		require.Positive(t, strings.Count(script, old),
			"sshdInstallCommand no longer contains %q; update this test with it", old)
		script = strings.ReplaceAll(script, old, replacement)
	}

	path := dir
	if withTimeout {
		path += string(os.PathListSeparator) + filepath.Dir(timeoutPath)
	}
	calls := filepath.Join(dir, "calls.log")
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + path, "CALLS=" + calls, "STATE=" + dir}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	run := sshdInstallRun{dir: dir}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		run.exitCode = exitErr.ExitCode()
	}
	run.stderr = stderr.String()
	if raw, err := os.ReadFile(calls); err == nil {
		run.calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return run
}

// A working apt installs on the first attempt, with no recovery step and no
// failure message.
func TestSSHDInstallCommandSucceedsFirstTime(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, "#!/bin/sh\necho \"apt-get $*\" >> \"$CALLS\"\n", true, false)

	require.Equal(t, 0, run.exitCode, run.stderr)
	require.Equal(t, 1, run.count(fakeCallUpdate))
	require.Equal(t, 1, run.count(fakeCallInstall))
	require.Equal(t, 0, run.count(fakeCallConfigure), "dpkg --configure -a runs only before a retry")
	require.NotContains(t, run.stderr, "install attempt")
}

// A first apt-get update that hangs is the #527 case. The fake hangs the way
// that defeats a SIGTERM-first timeout: it would exit on SIGTERM, but a helper
// it started ignores SIGTERM, and timeout escalates to SIGKILL only while its
// own child lives, so the helper would outlive the attempt and could hold a
// lock against the next one. The pid checks are the point: under -s KILL the
// whole process group dies at the limit. The helper's stdio is detached, as a
// daemonising helper's would be; on the test's stderr pipe it would hold
// cmd.Run open until it exited, and the survival would go unseen.
func TestSSHDInstallCommandKillsHungAttemptAndRetries(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, `#!/bin/sh
echo "apt-get $*" >> "$CALLS"
case "$*" in
*" update")
  if [ ! -e "$STATE/hung" ]; then
    ( trap '' TERM; exec /bin/sleep 60 ) </dev/null >/dev/null 2>&1 &
    echo $! > "$STATE/helper"
    echo $$ > "$STATE/hung"
    exec /bin/sleep 60
  fi ;;
esac
`, true, false)

	require.Equal(t, 0, run.exitCode, run.stderr)
	require.Equal(t, 2, run.count(fakeCallUpdate))
	require.Equal(t, 1, run.count(fakeCallInstall))
	require.Equal(t, 1, run.count(fakeCallConfigure))
	require.Contains(t, run.stderr, "sshd install attempt 1 of 3 failed or timed out")

	for _, name := range []string{"hung", "helper"} {
		raw, err := os.ReadFile(filepath.Join(run.dir, name))
		require.NoError(t, err)
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		require.NoError(t, err)
		require.Eventually(t, func() bool { return processGone(pid) },
			5*time.Second, 50*time.Millisecond, "the hung %s (pid %d) survived its attempt", name, pid)
	}
}

// An install interrupted by its timeout leaves dpkg half-configured, and apt
// refuses to install again until dpkg --configure -a has run.
func TestSSHDInstallCommandRecoversInterruptedInstall(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, `#!/bin/sh
echo "apt-get $*" >> "$CALLS"
case "$*" in
*" install "*)
  [ -e "$STATE/half-installed" ] && exit 100
  if [ ! -e "$STATE/interrupted" ]; then
    : > "$STATE/interrupted"
    : > "$STATE/half-installed"
    exec /bin/sleep 60
  fi ;;
esac
`, true, false)

	require.Equal(t, 0, run.exitCode, run.stderr)
	require.Equal(t, 2, run.count(fakeCallInstall))
	require.Equal(t, 1, run.count(fakeCallConfigure))
}

// apt normally starts dpkg in a separate session for its PTY, even without
// an interactive terminal. Model that session boundary with setsid: removing
// Dpkg::Use-Pty=0 must leave the fake dpkg alive and fail the PID assertion.
func TestSSHDInstallCommandKeepsDpkgInTimeoutGroup(t *testing.T) {
	t.Parallel()
	requireHostTool(t, "setsid", "util-linux setsid")
	run := runSSHDInstall(t, `#!/bin/sh
echo "apt-get $*" >> "$CALLS"
case "$*" in
*" install "*)
  [ -e "$STATE/half-installed" ] && exit 100
  if [ ! -e "$STATE/dpkg-pid" ]; then
    : > "$STATE/half-installed"
    case " $* " in
      *" -o Dpkg::Use-Pty=0 "*) /bin/sleep 60 </dev/null >/dev/null 2>&1 & ;;
      *) setsid /bin/sleep 60 </dev/null >/dev/null 2>&1 & ;;
    esac
    echo $! > "$STATE/dpkg-pid"
    exec /bin/sleep 60
  fi ;;
esac
`, true, false)

	raw, err := os.ReadFile(filepath.Join(run.dir, "dpkg-pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	// Also clean up the deliberately detached child if this regresses.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	require.Equal(t, 0, run.exitCode, run.stderr)
	require.Eventually(t, func() bool { return processGone(pid) },
		5*time.Second, 50*time.Millisecond, "dpkg (pid %d) escaped the timeout process group", pid)
	require.Equal(t, 2, run.count(fakeCallInstall))
	require.Equal(t, 1, run.count(fakeCallConfigure))
}

// An apt that always fails uses all three attempts, runs dpkg --configure -a
// before each retry, and exits 1 with the last attempt named.
func TestSSHDInstallCommandGivesUpAfterThreeAttempts(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, "#!/bin/sh\necho \"apt-get $*\" >> \"$CALLS\"\nexit 100\n", true, false)

	require.Equal(t, 1, run.exitCode)
	require.Equal(t, 3, run.count(fakeCallUpdate))
	require.Equal(t, 0, run.count(fakeCallInstall))
	require.Equal(t, 2, run.count(fakeCallConfigure))
	require.Contains(t, run.stderr, "sshd install attempt 3 of 3 failed or timed out")
}

// An image that ships sshd never reaches apt.
func TestSSHDInstallCommandSkipsPrebakedSSHD(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, "#!/bin/sh\necho \"apt-get $*\" >> \"$CALLS\"\n", true, true)

	require.Equal(t, 0, run.exitCode, run.stderr)
	require.Empty(t, run.calls)
}

// Without timeout the install would be unbounded, so the command refuses to
// start it and names the fix instead.
func TestSSHDInstallCommandRefusesWithoutTimeout(t *testing.T) {
	t.Parallel()
	run := runSSHDInstall(t, "#!/bin/sh\necho \"apt-get $*\" >> \"$CALLS\"\n", false, false)

	require.Equal(t, 1, run.exitCode)
	require.Empty(t, run.calls)
	require.Contains(t, run.stderr, "has no apt-get or timeout to install it; use an image that ships")
}
