//go:build !linux

package main

import (
	"io"
	"net"
)

const isSingleReactor = false

func tuneRuntimeForContainer() {}

func (s *Server) Listen(addr string) (int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	s.listener = ln
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func(nc net.Conn) {
			defer nc.Close()
			c := newConnState()
			for {
				c.prepareReadWindow()
				n, rerr := nc.Read(c.buf[c.end:])
				if n > 0 {
					c.end += n
					// Serialize stepConn + ForgeStore access across connections on non-Linux fallback
					// to match the single-threaded Linux epoll reactor invariants 100%.
					s.fallbackMu.Lock()
					var toWrite [][]byte
					var closeConnNow bool
					for c.end > c.start {
						resp, needMore, _, closeAfter, fatal := s.stepConn(c)
						if fatal {
							closeConnNow = true
							break
						}
						if len(resp) > 0 {
							// Copy response if it aliases c.wbuf and more pipelined requests remain
							if len(toWrite) > 0 || (c.end > c.start && !closeAfter) {
								cp := make([]byte, len(resp))
								copy(cp, resp)
								toWrite = append(toWrite, cp)
							} else {
								toWrite = append(toWrite, resp)
							}
						}
						if closeAfter {
							closeConnNow = true
							break
						}
						if needMore {
							break
						}
					}
					s.fallbackMu.Unlock()

					for _, pkt := range toWrite {
						if _, werr := nc.Write(pkt); werr != nil {
							return
						}
					}
					if closeConnNow {
						return
					}
				}
				if rerr != nil {
					if rerr == io.EOF {
						return
					}
					if n == 0 {
						return
					}
				}
			}
		}(conn)
	}
}

func (s *Server) Close() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
}
