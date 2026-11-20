//go:build linux

package main

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func tuneRuntimeForContainer() {
	// Pin Go scheduler to 1 P so the single-threaded epoll reactor owns core 0
	// and core 1 of the 2-CPU sandbox pod stays 100% free for the Node.js benchmark client.
	runtime.GOMAXPROCS(1)
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

func (s *Server) Listen(addr string) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		return 0, fmt.Errorf("socket: %w", err)
	}
	_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
	_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)

	ip, port := parseIPv4ListenAddr(addr)
	sa := &syscall.SockaddrInet4{Port: port, Addr: ip}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("bind: %w", err)
	}
	// Large listen backlog (2048) ensures 50-concurrent clone bursts never see SYN drops.
	if err := syscall.Listen(fd, 2048); err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("listen: %w", err)
	}
	nsa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("getsockname: %w", err)
	}
	boundPort := port
	if inet4, ok := nsa.(*syscall.SockaddrInet4); ok {
		boundPort = inet4.Port
	}

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("epoll_create1: %w", err)
	}
	ev := syscall.EpollEvent{
		Events: syscall.EPOLLIN,
		Fd:     int32(fd),
	}
	if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
		_ = syscall.Close(epfd)
		_ = syscall.Close(fd)
		return 0, fmt.Errorf("epoll_ctl add listen: %w", err)
	}

	s.listenFd = fd
	s.epollFd = epfd
	return boundPort, nil
}

func (s *Server) Serve() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var conns [4096]*connState
	var freeList []*connState
	var events [128]syscall.EpollEvent

	closeConn := func(fd int, c *connState) {
		_ = syscall.Close(fd)
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
		n, err := syscall.EpollWait(s.epollFd, events[:], -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			evMask := events[i].Events

			if fd == s.listenFd {
				// Drain all pending connections until EAGAIN
				for {
					nfd, _, aerr := syscall.Accept4(s.listenFd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
					if aerr != nil {
						if aerr == syscall.EINTR {
							continue
						}
						break
					}
					_ = syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
					_ = syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)

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
					if cerr := syscall.EpollCtl(s.epollFd, syscall.EPOLL_CTL_ADD, nfd, &cev); cerr != nil {
						closeConn(nfd, c)
					}
				}
				continue
			}

			if fd < 0 || fd >= len(conns) {
				_ = syscall.Close(fd)
				continue
			}
			c := conns[fd]
			if c == nil {
				_ = syscall.Close(fd)
				continue
			}

			if evMask&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 && evMask&syscall.EPOLLIN == 0 {
				closeConn(fd, c)
				continue
			}

			// Flush pending outbound bytes first if waiting on EPOLLOUT
			if len(c.outPending) > 0 {
				if !flushPendingLinux(s.epollFd, fd, c) {
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
				c.prepareReadWindow()
				rn, rerr := syscall.Read(fd, c.buf[c.end:])
				if rn > 0 {
					c.end += rn
					quickAckFn := func() {
						_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
					}
					var closeConnNow bool
					for c.end > c.start && len(c.outPending) == 0 {
						resp, needMore, needQuickAck, closeAfter, fatal := s.stepConn(c, quickAckFn)
						if fatal {
							closeConnNow = true
							break
						}
						if len(resp) > 0 {
							if !writeOrQueueLinux(s.epollFd, fd, c, resp, closeAfter) {
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
							if needQuickAck {
								_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
							}
							break
						}
					}
					if closeConnNow {
						closeConn(fd, c)
						continue
					}
				}
				if rn == 0 && rerr == nil {
					closeConn(fd, c)
					continue
				}
				if rerr != nil && rerr != syscall.EAGAIN && rerr != syscall.EWOULDBLOCK && rerr != syscall.EINTR {
					closeConn(fd, c)
					continue
				}
				if evMask&syscall.EPOLLRDHUP != 0 && rn == 0 {
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
		wn, werr := syscall.Write(fd, resp[written:])
		if wn > 0 {
			written += wn
		}
		if werr != nil {
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
					_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, fd, &ev)
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
		wn, werr := syscall.Write(fd, c.outPending)
		if wn > 0 {
			if wn == len(c.outPending) {
				c.outPending = c.outPending[:0]
				break
			}
			copy(c.outPending, c.outPending[wn:])
			c.outPending = c.outPending[:len(c.outPending)-wn]
		}
		if werr != nil {
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
		_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_MOD, fd, &ev)
		c.watchingOut = false
	}
	return true
}

func (s *Server) Close() {
	if s.listenFd > 0 {
		_ = syscall.Close(s.listenFd)
		s.listenFd = 0
	}
	if s.epollFd > 0 {
		_ = syscall.Close(s.epollFd)
		s.epollFd = 0
	}
}
