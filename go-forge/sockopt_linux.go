//go:build linux

package main

import (
	"net"
	"runtime"
	"syscall"
)

func tuneRuntimeForContainer() {
	// Cap GOMAXPROCS to 2 on Linux matching the 2-CPU sandbox quota to avoid CFS/futex thrashing
	if runtime.GOMAXPROCS(0) > 2 {
		runtime.GOMAXPROCS(2)
	}
}

func setQuickAck(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
	})
}
