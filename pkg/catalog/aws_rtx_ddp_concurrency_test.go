// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the rendered writer against the small inspected function fixture,
// substituting its hashes rather than vendoring the entire upstream module.
func TestAWSRTXDDPGuardSharedWorkspace(t *testing.T) {
	python, err := exec.LookPath("python3")
	require.NoError(t, err)
	for _, mode := range []string{"concurrent", "replace-failure", "lock-unavailable", "lock-unavailable-concurrent"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "training.py")
			script := awsRTXDDPGuardForPath(awsRTXDDPGuardScript(t), path)
			scriptPath := filepath.Join(dir, "guard.py")
			require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o600))
			fixture, err := filepath.Abs("testdata/aws-rtx-ddp-guard/get_model.py")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, python, "-c", awsRTXDDPSharedWorkspaceHarness,
				scriptPath, fixture, path, mode).CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

const awsRTXDDPSharedWorkspaceHarness = `
import fcntl
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import sys

script_path, fixture, target, mode = sys.argv[1:]
path = Path(target)
expected = Path(fixture).read_text()
before = "        with torch.cuda.stream(torch.cuda.Stream()):\n            model = [\n"
after = (
    "        # Order parameter initialization, DDP copies, and optimizer setup.\n"
    "        current_stream = torch.cuda.current_stream()\n"
    "        ddp_stream = torch.cuda.Stream()\n"
    "        ddp_stream.wait_stream(current_stream)\n"
    "        with torch.cuda.stream(ddp_stream):\n"
    "            model = [\n"
)
assert expected.count(after) == 1
original = expected.replace(after, before).replace("        current_stream.wait_stream(ddp_stream)\n\n", "", 1)
path.write_text(original)
path.chmod(0o640)
script = Path(script_path).read_text()
for name, content in (("original", original), ("patched", expected)):
    digest = hashlib.sha256(content.encode()).hexdigest()
    script, count = re.subn(name + r' = "[a-f0-9]{64}"', name + ' = "' + digest + '"', script)
    assert count == 1
Path(script_path).write_text(script)

wrapper = r'''
import fcntl
import os
from pathlib import Path
import stat
import sys

script, target, fixture, mode = sys.argv[1:]
original = Path(target).read_text()
expected = Path(fixture).read_text()
real_flock = fcntl.flock
def announced_lock(fd, operation):
    print("WAITING", flush=True)
    if mode.startswith("lock-unavailable"):
        raise OSError("simulated unsupported workspace locking")
    return real_flock(fd, operation)
fcntl.flock = announced_lock
real_replace = os.replace
replacements = []
def checked_replace(source, dest):
    assert Path(source).parent == Path(dest).parent
    assert Path(source).read_text() == expected
    assert Path(dest).read_text() == original
    assert stat.S_IMODE(Path(source).stat().st_mode) == 0o640
    if mode == "replace-failure":
        raise OSError("simulated replacement failure")
    if mode == "lock-unavailable-concurrent":
        print("READY", flush=True)
        assert sys.stdin.readline().strip() == "replace"
    replacements.append(True)
    return real_replace(source, dest)
os.replace = checked_replace
exec(Path(script).read_text(), {})
print("REPLACEMENTS=" + str(len(replacements)), flush=True)
'''

if mode == "concurrent":
    lock_path = path.parent / ".megatron-lm.lock"
    with lock_path.open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        workers = [subprocess.Popen([sys.executable, "-c", wrapper, script_path, target, fixture, mode],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) for _ in range(2)]
        # Each process announces its attempt before the real flock blocks.
        # Both must leave the file untouched until this holder releases it.
        for worker in workers:
            assert worker.stdout.readline().strip() == "WAITING"
            assert worker.poll() is None
        assert path.read_text() == original
        fcntl.flock(lock, fcntl.LOCK_UN)
        outputs = []
        for worker in workers:
            stdout, stderr = worker.communicate(timeout=20)
            assert worker.returncode == 0, (stdout, stderr)
            outputs.append(stdout)
    assert sum("Applied Megatron" in output for output in outputs) == 1, outputs
    assert sum("already applied" in output for output in outputs) == 1, outputs
    assert sum("REPLACEMENTS=1" in output for output in outputs) == 1, outputs
    assert path.read_text() == expected
elif mode == "lock-unavailable-concurrent":
    workers = [subprocess.Popen([sys.executable, "-c", wrapper, script_path, target, fixture, mode],
                               stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, text=True) for _ in range(2)]
    outputs = []
    for worker in workers:
        lines = []
        while True:
            line = worker.stdout.readline()
            assert line, "guard exited before its atomic replacement"
            lines.append(line)
            if line.strip() == "READY":
                break
        outputs.append("".join(lines))
    # Both writers verified the original digest and staged identical complete
    # files. Neither replacement is allowed until both reach this barrier.
    assert path.read_text() == original
    for worker in workers:
        worker.stdin.write("replace\n")
        worker.stdin.flush()
    for worker, prefix in zip(workers, outputs):
        stdout, stderr = worker.communicate(timeout=20)
        assert worker.returncode == 0, (prefix + stdout, stderr)
        assert "preparing without the lock" in prefix
        assert "Applied Megatron" in stdout
        assert "REPLACEMENTS=1" in stdout
    assert path.read_text() == expected
elif mode == "lock-unavailable":
    for replacements in (1, 0):
        run = subprocess.run([sys.executable, "-c", wrapper, script_path, target, fixture, mode],
                             capture_output=True, text=True, timeout=20)
        assert run.returncode == 0, run
        assert "preparing without the lock" in run.stdout
        assert "REPLACEMENTS=" + str(replacements) in run.stdout
        assert path.read_text() == expected
else:
    run = subprocess.run([sys.executable, "-c", wrapper, script_path, target, fixture, mode],
                         capture_output=True, text=True, timeout=20)
    assert run.returncode != 0 and "simulated replacement failure" in run.stderr, run
    assert path.read_text() == original
assert stat.S_IMODE(path.stat().st_mode) == 0o640
assert not list(path.parent.glob(".training.py.*"))
`
