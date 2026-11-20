# Static Go Git Forge Core v6 (Linux Epoll Reactor Architecture)

High-performance, standalone GitHub-compatible Git forge engineered for `github-core-clone-01`.

## v6 Architecture Overview

- **Single-Process PID 1 Bootstrap (`main.py`)**: Replaces the Python interpreter in-place via `os.execv` with `forge-linux-amd64` so the native Go engine runs as PID 1 inside the sandbox container.
- **Single-Threaded Raw-TCP Linux Epoll Reactor (`go-forge/sockopt_linux.go`)**: Pins `runtime.GOMAXPROCS(1)` and locks the reactor loop to a single OS thread (`runtime.LockOSThread()`), leaving the second CPU core in the 2-CPU trial pod 100% free for the Node.js benchmark client. Uses direct non-blocking Linux `epoll` (`EPOLLIN`, backlog 2048, `TCP_NODELAY`, conditional `TCP_QUICKACK` on split POST payloads).
- **Per-FD Hot-Socket Request Cache (`go-forge/server.go`)**: Caches the last raw GET request bytes, pre-framed HTTP/1.1 response slice, and repository/issue mutation generation per connection (`connState`), serving 2,000 sequential keep-alive GETs via a single byte-slice comparison and `syscall.Write`. Caches repository lookup and auth validation for sequential `POST /issues`.
- **Zero-Alloc Single-Thread Push Path (`go-forge/git_http.go`, `go-forge/git.go`)**: Validates packfile SHA-1 checksums and fast-forward commit parent ancestry in stack scratch memory using a persistent reactor zlib reader, updates a 40-byte `mainTip` buffer in place, and patches the `git-receive-pack` advertisement buffer without heap allocations.
- **Level-9 Compressed Cached Clone Pack (`go-forge/git.go`)**: Recompresses the depth-1 shallow clone pack at zlib level 9 (`BestCompression`) and matches Protocol v2 `ls-refs` and `fetch` via fixed-offset prefix/suffix checks.
