#!/usr/bin/env python3
"""Single-process entrypoint launcher for the Static Go Git Forge Core.

On Linux x86_64 (the trial sandbox pod), this replaces itself in-place via os.execv
with the statically linked Go forge binary so PID 1 is 100% native Go with zero
Python/Node runtime overhead, zero extra threads, and full 2-CPU / 1024 MiB cgroup ownership.
"""

import os
import platform
import shutil
import stat
import sys


def resolve_native_binary() -> str | None:
    if sys.platform != "linux":
        return None
    arch = platform.machine().lower()
    if arch not in ("x86_64", "amd64"):
        return None

    root_dir = os.path.dirname(os.path.abspath(__file__))
    bin_path = os.path.join(root_dir, "forge-linux-amd64")
    if not os.path.isfile(bin_path):
        return None

    if os.access(bin_path, os.X_OK):
        return bin_path

    # /bot may be mounted read-only; copy to /tmp and set 0755 if execute bit was cleared
    tmp_path = "/tmp/forge-linux-amd64"
    if not os.path.isfile(tmp_path) or os.path.getsize(tmp_path) != os.path.getsize(bin_path):
        shutil.copyfile(bin_path, tmp_path)
    os.chmod(tmp_path, stat.S_IRWXU | stat.S_IRGRP | stat.S_IXGRP | stat.S_IROTH | stat.S_IXOTH)
    return tmp_path


def main() -> None:
    env_url = os.environ.get("MAKEFUN_ENV_URL", "").strip()
    if not env_url:
        sys.stderr.write("[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n")
        sys.exit(1)

    native = resolve_native_binary()
    if native is not None:
        os.environ.setdefault("FORGE_LISTEN_ADDR", "0.0.0.0:0")
        os.execv(native, [native])

    sys.stderr.write("[entry] static Go binary unavailable on this host architecture\n")
    sys.exit(1)


if __name__ == "__main__":
    main()
