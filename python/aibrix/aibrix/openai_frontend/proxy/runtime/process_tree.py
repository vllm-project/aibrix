# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Process-tree bookkeeping for the inner engine of :class:`ProxyEngine`.

The inner engine (vLLM, SGLang, ...) is not a single process: the API
server forks an engine core, which forks one worker per GPU, and some of
those call ``setsid()`` or outlive their parent.  Killing only the root,
or only its process group, leaks GPU-holding orphans that make the next
start OOM.

The approach follows the teardown procedure in ArcticInference's
semi-persistence worker:

* **Identity, not bare pids.**  A tree member is ``(pid, create_time)``.
  A recycled pid always reports a later create time, so a stale snapshot
  can never make us signal an unrelated process.
* **Snapshot while the tree is known-live.**  Membership is recorded
  periodically while the root is alive.  When the root crashes its
  children are reparented to init and vanish from any live walk; the
  snapshot is what still finds them.
* **Never signal anything we do not own.**  No pid <= 1, nothing in our
  own ancestry, and never a process group that is our own.
* **Graceful, then forceful.**  SIGTERM, wait up to a grace period,
  SIGKILL survivors leaves-first, then report anything still alive
  (typically a task stuck in uninterruptible GPU-driver sleep).
* **Forensic exit descriptions.**  Decode the exit status into
  ``killed_by=SIGKILL(9) (likely kernel OOM-killer)`` and friends instead
  of a bare return code.

``psutil`` is optional.  Without it, teardown falls back to the process
group alone, which still covers every descendant that did not call
``setsid()``.
"""

from __future__ import annotations

import logging
import os
import signal
import time
from dataclasses import dataclass, field
from typing import Dict, List, Optional

try:  # pragma: no cover - exercised implicitly by the environment
    import psutil
except ImportError:  # pragma: no cover
    psutil = None  # type: ignore[assignment]

logger = logging.getLogger(__name__)

HAS_PSUTIL = psutil is not None

# Hints for signals that most often explain an inference-engine death.
_SIGNAL_HINTS = {
    signal.SIGKILL: "likely kernel OOM-killer",
    signal.SIGSEGV: "native/CUDA crash",
    signal.SIGABRT: "assert/abort",
    signal.SIGBUS: "bus error, often /dev/shm exhaustion",
    signal.SIGTERM: "terminated",
}


def _signal_desc(signum: int) -> str:
    try:
        name = signal.Signals(signum).name
    except ValueError:
        name = f"sig{signum}"
    hint = _SIGNAL_HINTS.get(signum)  # type: ignore[call-overload]
    return f"{name}({signum})" + (f" ({hint})" if hint else "")


def describe_returncode(returncode: Optional[int]) -> str:
    """Return a one-line description of a ``Popen.returncode``.

    Distinguishes a signal death (negative return code) from a clean
    exit, and recognises the ``128 + signum`` convention a shell wrapper
    uses to report that *its* child was killed by a signal.
    """
    if returncode is None:
        return "running"
    if returncode < 0:
        return f"killed_by={_signal_desc(-returncode)}"
    desc = f"exited_code={returncode}"
    if 128 < returncode < 128 + 65:
        desc += f" (128+{_signal_desc(returncode - 128)})"
    return desc


def _create_time(pid: int) -> Optional[float]:
    if not HAS_PSUTIL:
        return None
    try:
        return psutil.Process(pid).create_time()
    except (psutil.NoSuchProcess, psutil.AccessDenied, psutil.ZombieProcess):
        return None


def _own_ancestry() -> set:
    """Pids of this process and its ancestors; never valid kill targets."""
    protected = {os.getpid()}
    if not HAS_PSUTIL:
        protected.add(os.getppid())
        return protected
    try:
        proc = psutil.Process()
        protected.update(p.pid for p in proc.parents())
    except psutil.Error:
        protected.add(os.getppid())
    return protected


@dataclass
class TreeSnapshot:
    """Identity snapshot of a process tree rooted at ``root_pid``."""

    root_pid: int
    pgid: Optional[int]
    # pid -> create_time (None when unknown, i.e. no psutil)
    members: Dict[int, Optional[float]] = field(default_factory=dict)
    rss_bytes: Optional[int] = None
    taken_at: float = 0.0

    @property
    def pids(self) -> List[int]:
        return sorted(self.members)


def snapshot_tree(
    root_pid: int, previous: Optional[TreeSnapshot] = None
) -> TreeSnapshot:
    """Record the identity of ``root_pid`` and all of its live descendants.

    Members of ``previous`` that are still alive with the same create time
    are carried over, so a worker that was reparented to init between two
    snapshots stays tracked.
    """
    try:
        pgid: Optional[int] = os.getpgid(root_pid)
    except OSError:
        pgid = previous.pgid if previous is not None else None

    snap = TreeSnapshot(root_pid=root_pid, pgid=pgid, taken_at=time.monotonic())
    if not HAS_PSUTIL:
        snap.members[root_pid] = None
        return snap

    procs = []
    try:
        root = psutil.Process(root_pid)
        procs = [root] + root.children(recursive=True)
    except (psutil.NoSuchProcess, psutil.ZombieProcess):
        procs = []
    except psutil.AccessDenied:
        procs = []

    rss = 0
    for p in procs:
        try:
            snap.members[p.pid] = p.create_time()
            rss += p.memory_info().rss
        except (psutil.NoSuchProcess, psutil.ZombieProcess, psutil.AccessDenied):
            continue
    snap.rss_bytes = rss or (previous.rss_bytes if previous is not None else None)

    if previous is not None:
        for pid, start in previous.members.items():
            if pid in snap.members:
                continue
            if start is not None and _create_time(pid) == start:
                snap.members[pid] = start
    return snap


def _is_same_process(pid: int, start: Optional[float]) -> bool:
    """True if ``pid`` is alive and (when known) still the same process."""
    if not HAS_PSUTIL:
        try:
            os.kill(pid, 0)
            return True
        except ProcessLookupError:
            return False
        except PermissionError:
            return True
    live = _create_time(pid)
    if live is None:
        return False
    return start is None or live == start


def _signal_group(pgid: Optional[int], sig: int) -> None:
    if pgid is None or pgid <= 1:
        return
    try:
        if pgid == os.getpgrp():
            # Our own group: the child never got its own session.  Signalling
            # it would kill this process, so rely on per-pid signals instead.
            return
        os.killpg(pgid, sig)
    except (ProcessLookupError, PermissionError):
        pass


def _group_exists(pgid: Optional[int]) -> bool:
    """Check the fallback group independently of the already-reaped root."""
    if pgid is None or pgid <= 1 or pgid == os.getpgrp():
        return False
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _signal_members(
    pids: List[int], snap: TreeSnapshot, sig: int, protected: set
) -> None:
    # Leaves first: the highest pids are, in practice, the deepest children.
    for pid in sorted(pids, reverse=True):
        if pid <= 1 or pid in protected:
            continue
        if not _is_same_process(pid, snap.members.get(pid)):
            continue
        try:
            os.kill(pid, sig)
        except (ProcessLookupError, PermissionError):
            pass


def _alive(pids: List[int], snap: TreeSnapshot) -> List[int]:
    alive = []
    for pid in pids:
        if not _is_same_process(pid, snap.members.get(pid)):
            continue
        if HAS_PSUTIL:
            try:
                if psutil.Process(pid).status() == psutil.STATUS_ZOMBIE:
                    continue
            except psutil.Error:
                continue
        alive.append(pid)
    return alive


def kill_tree(
    snap: TreeSnapshot,
    grace_seconds: float = 10.0,
    reap=None,
    log: Optional[logging.Logger] = None,
) -> List[int]:
    """Terminate every process in ``snap`` (plus late-forked descendants).

    ``reap`` is an optional callable taking a timeout that waits for (and
    reaps) the root, normally ``Popen.wait``; without it the root would be
    left as a zombie and look alive.

    Returns the pids that are still alive after SIGKILL. Without psutil, a
    surviving group's PGID is included because its members cannot be enumerated.
    """
    log = log or logger
    protected = _own_ancestry()

    # Pick up anything forked since the last snapshot, but only while the
    # root is provably still ours.
    if _is_same_process(snap.root_pid, snap.members.get(snap.root_pid)):
        snap = snapshot_tree(snap.root_pid, previous=snap)

    targets = [p for p in snap.members if p > 1 and p not in protected]
    log.info(
        "Terminating inner engine tree root=%s pgid=%s members=%s",
        snap.root_pid,
        snap.pgid,
        sorted(targets),
    )

    def _wait(timeout: float) -> List[int]:
        deadline = time.monotonic() + max(timeout, 0.0)
        while True:
            if reap is not None:
                try:
                    reap(0)
                except Exception:
                    pass
            remaining = _alive(targets, snap)
            if not HAS_PSUTIL and _group_exists(snap.pgid):
                # The root may be gone while same-group workers still live.
                assert snap.pgid is not None
                if snap.pgid not in remaining:
                    remaining.append(snap.pgid)
            if not remaining or time.monotonic() >= deadline:
                return remaining
            time.sleep(0.05)

    # With identities available, never signal a stale/recycled process group:
    # every target must pass the per-process create-time check below.
    if not HAS_PSUTIL:
        _signal_group(snap.pgid, signal.SIGTERM)
    _signal_members(targets, snap, signal.SIGTERM, protected)
    survivors = _wait(grace_seconds)
    if not survivors:
        return []

    log.warning(
        "Inner engine tree did not exit within %.1fs; SIGKILL %s",
        grace_seconds,
        survivors,
    )
    if not HAS_PSUTIL:
        _signal_group(snap.pgid, signal.SIGKILL)
        _signal_members(targets, snap, signal.SIGKILL, protected)
    else:
        _signal_members(survivors, snap, signal.SIGKILL, protected)
    survivors = _wait(5.0)
    if survivors:
        log.error(
            "Inner engine processes survived SIGKILL (uninterruptible sleep, "
            "e.g. stuck in the GPU driver?): %s",
            survivors,
        )
    return survivors
