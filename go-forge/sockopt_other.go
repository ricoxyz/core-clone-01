//go:build !linux

package main

import "net"

func tuneRuntimeForContainer() {}

func setQuickAck(conn net.Conn) {}
