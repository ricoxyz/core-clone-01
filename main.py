#!/usr/bin/env python3
"""Native Linux amd64 process bootstrap for GitHub Core Forge v6 (Epoll Reactor Edition).

Replaces the Python interpreter process in-place via os.execv with the static Go v6 engine
so the single-threaded Linux epoll reactor runs directly as PID 1 with zero wrapper overhead.
"""

import os
import platform
import shutil
import stat
import sys

V6_BINARY_NAME = "forge-linux-amd64"
V6_FALLBACK_TMP = "/tmp/forge-v6-epoll-amd64"
V6_DEFAULT_BIND = "0.0.0.0:0"


def locate_v6_reactor_binary() -> str:
    if sys.platform != "linux":
        sys.stderr.write("[v6-boot] fatal: Linux kernel required for epoll reactor binary\n")
        sys.exit(1)

    machine_arch = platform.machine().lower()
    if machine_arch != "x86_64" and machine_arch != "amd64":
        sys.stderr.write(f"[v6-boot] fatal: unsupported CPU architecture {machine_arch}\n")
        sys.exit(1)

    repo_root = os.path.dirname(os.path.abspath(__file__))
    embedded_bin = os.path.join(repo_root, V6_BINARY_NAME)
    if not os.path.isfile(embedded_bin):
        sys.stderr.write(f"[v6-boot] fatal: missing binary at {embedded_bin}\n")
        sys.exit(1)

    if os.access(embedded_bin, os.X_OK):
        return embedded_bin

    source_size = os.path.getsize(embedded_bin)
    need_copy = True
    if os.path.isfile(V6_FALLBACK_TMP):
        if os.path.getsize(V6_FALLBACK_TMP) == source_size:
            need_copy = False

    if need_copy:
        shutil.copyfile(embedded_bin, V6_FALLBACK_TMP)

    exec_perms = stat.S_IRWXU | stat.S_IRGRP | stat.S_IXGRP | stat.S_IROTH | stat.S_IXOTH
    os.chmod(V6_FALLBACK_TMP, exec_perms)
    return V6_FALLBACK_TMP


def bootstrap_v6_forge() -> None:
    suite_env_url = os.environ.get("MAKEFUN_ENV_URL", "").strip()
    if not suite_env_url:
        sys.stderr.write("[v6-boot] fatal: MAKEFUN_ENV_URL environment variable is not set\n")
        sys.exit(1)

    reactor_exec_path = locate_v6_reactor_binary()
    os.environ.setdefault("FORGE_LISTEN_ADDR", V6_DEFAULT_BIND)
    os.execv(reactor_exec_path, [reactor_exec_path])


if __name__ == "__main__":
    bootstrap_v6_forge()
