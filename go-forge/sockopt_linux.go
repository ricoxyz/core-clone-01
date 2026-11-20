//go:build linux

package main

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const soReusePort = 15

func tuneRuntimeForContainer() {
	// Run 2 Ps on the 2-CPU sandbox pod, one per OS-thread-pinned epoll reactor.
	runtime.GOMAXPROCS(2)
}

func parseIPv4ListenAddr(addr string) ([4]byte, int) {
	ip := [4]byte{0, 0, 0, 0}
	port := 0
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return ip, 0
	}
	if p, perr := strconv.Atoi(portStr); perr == nil && p >= 0 && p <= 65535 {
		port = p
	}
	host = strings.TrimSpace(host)
	if host != "" && host != "0.0.0.0" && host != "::" {
		if parsed := net.ParseIP(host); parsed != nil {
			if v4 := parsed.To4(); v4 != nil {
				copy(ip[:], v4)
			}
		}
	}
	return ip, port
}

func openReusePortListener(ip [4]byte, port int) (fd int, epfd int, boundPort int, err error) {
	fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("socket: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if rerr := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1); rerr != nil {
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("setsockopt SO_REUSEPORT: %w", rerr)
	}
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)

	sa := &syscall.SockaddrInet4{Port: port, Addr: ip}
	if berr := syscall.Bind(fd, sa); berr != nil {
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("bind: %w", berr)
	}
	// Large listen backlog (2048) on each SO_REUSEPORT socket prevents SYN drops on 50-way clone bursts.
	if lerr := syscall.Listen(fd, 2048); lerr != nil {
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("listen: %w", lerr)
	}
	nsa, gerr := syscall.Getsockname(fd)
	if gerr != nil {
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("getsockname: %w", gerr)
	}
	boundPort = port
	if inet4, ok := nsa.(*syscall.SockaddrInet4); ok {
		boundPort = inet4.Port
	}

	epfd, err = syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("epoll_create1: %w", err)
	}
	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(fd),
	}
	if cerr := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); cerr != nil {
		_ = syscall.Close(epfd)
		_ = syscall.Close(fd)
		return 0, 0, 0, fmt.Errorf("epoll_ctl add listen: %w", cerr)
	}
	return fd, epfd, boundPort, nil
}

func (s *Server) Listen(addr string) (int, error) {
	ip, port := parseIPv4ListenAddr(addr)
	fd0, ep0, boundPort, err := openReusePortListener(ip, port)
	if err != nil {
		return 0, err
	}
	fd1, ep1, _, err := openReusePortListener(ip, boundPort)
	if err != nil {
		_ = syscall.Close(ep0)
		_ = syscall.Close(fd0)
		return 0, err
	}

	s.listenFds[0] = fd0
	s.epollFds[0] = ep0
	s.listenFds[1] = fd1
	s.epollFds[1] = ep1
	return boundPort, nil
}

func rawRead(fd int, buf []byte) (int, syscall.Errno) {
	if len(buf) == 0 {
		return 0, 0
	}
	r1, _, err := syscall.RawSyscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if err != 0 {
		return -1, err
	}
	return int(r1), 0
}

func rawWrite(fd int, buf []byte) (int, syscall.Errno) {
	if len(buf) == 0 {
		return 0, 0
	}
	r1, _, err := syscall.RawSyscall(syscall.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if err != 0 {
		return -1, err
	}
	return int(r1), 0
}

func rawAccept4(listenFd int) (int, syscall.Errno) {
	r1, _, err := syscall.RawSyscall6(syscall.SYS_ACCEPT4, uintptr(listenFd), 0, 0, uintptr(syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC), 0, 0)
	if err != 0 {
		return -1, err
	}
	return int(r1), 0
}

func rawSetTCPOpt(fd int, opt int) {
	var one int32 = 1
	_, _, _ = syscall.RawSyscall6(syscall.SYS_SETSOCKOPT, uintptr(fd), uintptr(syscall.IPPROTO_TCP), uintptr(opt), uintptr(unsafe.Pointer(&one)), 4, 0)
}

func rawEpollCtl(epfd, op, fd int, ev *syscall.EpollEvent) syscall.Errno {
	_, _, err := syscall.RawSyscall6(syscall.SYS_EPOLL_CTL, uintptr(epfd), uintptr(op), uintptr(fd), uintptr(unsafe.Pointer(ev)), 0, 0)
	return err
}

func rawClose(fd int) {
	_, _, _ = syscall.RawSyscall(syscall.SYS_CLOSE, uintptr(fd), 0, 0)
}

func (s *Server) Serve() {
	lfd0, epfd0 := s.listenFds[0], s.epollFds[0]
	lfd1, epfd1 := s.listenFds[1], s.epollFds[1]

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.serveReactor(lfd1, epfd1)
	}()
	s.serveReactor(lfd0, epfd0)
	wg.Wait()
}

func (s *Server) serveReactor(listenFd, epollFd int) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var conns [4096]*connState
	connSlab := make([]connState, 128)
	freeList := make([]*connState, 128)
	for i := range connSlab {
		connSlab[i].fd = -1
		connSlab[i].buf = make([]byte, 4096)
		connSlab[i].wbuf = make([]byte, 0, 2048)
		freeList[i] = &connSlab[i]
	}
	var events [128]syscall.EpollEvent

	closeConn := func(fd int, c *connState) {
		rawClose(fd)
		if fd >= 0 && fd < len(conns) {
			conns[fd] = nil
		}
		if c != nil {
			c.resetForPool()
			if len(freeList) < 128 {
				freeList = append(freeList, c)
			}
		}
	}

	for {
		n, err := syscall.EpollWait(epollFd, events[:], -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			evMask := events[i].Events

			if fd == listenFd {
				// Drain all queued connections until EAGAIN; set TCP_NODELAY once on accept.
				for {
					nfd, aerr := rawAccept4(listenFd)
					if aerr != 0 {
						if aerr == syscall.EINTR {
							continue
						}
						break
					}
					rawSetTCPOpt(nfd, syscall.TCP_NODELAY)

					var c *connState
					if l := len(freeList); l > 0 {
						c = freeList[l-1]
						freeList = freeList[:l-1]
					} else {
						c = newConnState()
					}
					c.fd = nfd
					if nfd >= 0 && nfd < len(conns) {
						conns[nfd] = c
					}
					cev := syscall.EpollEvent{
						Events: syscall.EPOLLIN | syscall.EPOLLRDHUP,
						Fd:     int32(nfd),
					}
					if cerr := rawEpollCtl(epollFd, syscall.EPOLL_CTL_ADD, nfd, &cev); cerr != 0 {
						closeConn(nfd, c)
					}
				}
				continue
			}

			if fd < 0 || fd >= len(conns) {
				rawClose(fd)
				continue
			}
			c := conns[fd]
			if c == nil {
				rawClose(fd)
				continue
			}

			if evMask&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 && evMask&syscall.EPOLLIN == 0 {
				closeConn(fd, c)
				continue
			}

			// Flush pending outbound bytes first if waiting on EPOLLOUT.
			if len(c.outPending) > 0 {
				if !flushPendingLinux(epollFd, fd, c) {
					closeConn(fd, c)
					continue
				}
				if len(c.outPending) > 0 {
					continue
				}
				if c.closeAfterFlush {
					closeConn(fd, c)
					continue
				}
			}

			if evMask&syscall.EPOLLIN != 0 {
				needQuickAckPending := false
				closeConnNow := false
				eof := false
				for {
					c.prepareReadWindow()
					rn, rerr := rawRead(fd, c.buf[c.end:])
					if rn > 0 {
						c.end += rn
						needQuickAckPending = false
						wantMoreRead := false
						for c.end > c.start && len(c.outPending) == 0 {
							s.storeMu.Lock()
							resp, needMore, needQuickAck, closeAfter, fatal := s.stepConn(c)
							s.storeMu.Unlock()

							if fatal {
								closeConnNow = true
								break
							}
							if len(resp) > 0 {
								if !writeOrQueueLinux(epollFd, fd, c, resp, closeAfter) {
									closeConnNow = true
									break
								}
								if closeAfter && len(c.outPending) == 0 {
									closeConnNow = true
									break
								}
							} else if closeAfter {
								closeConnNow = true
								break
							}
							if needMore {
								wantMoreRead = true
								needQuickAckPending = needQuickAck
								break
							}
						}
						if closeConnNow || len(c.outPending) > 0 || !wantMoreRead {
							break
						}
						continue
					}
					if rn == 0 && rerr == 0 {
						eof = true
						break
					}
					if rerr == syscall.EINTR {
						continue
					}
					if rerr == syscall.EAGAIN || rerr == syscall.EWOULDBLOCK {
						if needQuickAckPending {
							rawSetTCPOpt(fd, syscall.TCP_QUICKACK)
						}
						break
					}
					closeConnNow = true
					break
				}
				if closeConnNow || eof {
					closeConn(fd, c)
					continue
				}
			}
		}
	}
}

func writeOrQueueLinux(epfd, fd int, c *connState, resp []byte, closeAfter bool) bool {
	written := 0
	for written < len(resp) {
		wn, werr := rawWrite(fd, resp[written:])
		if wn > 0 {
			written += wn
		}
		if werr != 0 {
			if werr == syscall.EINTR {
				continue
			}
			if werr == syscall.EAGAIN || werr == syscall.EWOULDBLOCK {
				c.outPending = append(c.outPending[:0], resp[written:]...)
				c.closeAfterFlush = closeAfter
				if !c.watchingOut {
					ev := syscall.EpollEvent{
						Events: syscall.EPOLLIN | syscall.EPOLLOUT | syscall.EPOLLRDHUP,
						Fd:     int32(fd),
					}
					_ = rawEpollCtl(epfd, syscall.EPOLL_CTL_MOD, fd, &ev)
					c.watchingOut = true
				}
				return true
			}
			return false
		}
	}
	return true
}

func flushPendingLinux(epfd, fd int, c *connState) bool {
	for len(c.outPending) > 0 {
		wn, werr := rawWrite(fd, c.outPending)
		if wn > 0 {
			if wn == len(c.outPending) {
				c.outPending = c.outPending[:0]
				break
			}
			copy(c.outPending, c.outPending[wn:])
			c.outPending = c.outPending[:len(c.outPending)-wn]
		}
		if werr != 0 {
			if werr == syscall.EINTR {
				continue
			}
			if werr == syscall.EAGAIN || werr == syscall.EWOULDBLOCK {
				return true
			}
			return false
		}
	}
	if c.watchingOut {
		ev := syscall.EpollEvent{
			Events: syscall.EPOLLIN | syscall.EPOLLRDHUP,
			Fd:     int32(fd),
		}
		_ = rawEpollCtl(epfd, syscall.EPOLL_CTL_MOD, fd, &ev)
		c.watchingOut = false
	}
	return true
}

func (s *Server) Close() {
	s.closeOnce.Do(func() {
		for i := 0; i < 2; i++ {
			if s.listenFds[i] > 0 {
				_ = syscall.Close(s.listenFds[i])
				s.listenFds[i] = 0
			}
			if s.epollFds[i] > 0 {
				_ = syscall.Close(s.epollFds[i])
				s.epollFds[i] = 0
			}
		}
	})
}
