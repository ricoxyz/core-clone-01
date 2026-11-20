package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestV9ServerListenServeAndCleanShutdown(t *testing.T) {
	store := NewForgeStore()
	store.CreateRepo("adaiversen", "v9-live", "test", false)
	srv := NewServer(store)

	port, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	if port <= 0 {
		t.Fatalf("expected ephemeral port > 0, got %d", port)
	}

	done := make(chan struct{})
	go func() {
		srv.Serve()
		close(done)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		srv.Close()
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// 1. Standard zero-reflection POST issue create
	postBody := `{"title":"v9 issue","body":"zero-futex fast path"}`
	postReq := fmt.Sprintf(
		"POST /api/v3/repos/adaiversen/v9-live/issues HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer tok\r\nContent-Length: %d\r\n\r\n%s",
		addr, len(postBody), postBody,
	)
	if _, err := conn.Write([]byte(postReq)); err != nil {
		t.Fatalf("write POST failed: %v", err)
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read POST resp failed: %v", err)
	}
	if !bytes.Contains(buf[:n], []byte("HTTP/1.1 201 Created")) || !bytes.Contains(buf[:n], []byte(`"title":"v9 issue"`)) {
		t.Fatalf("unexpected POST response: %s", string(buf[:n]))
	}

	// 2. Keep-alive GET issue read (populates per-FD cache)
	getReq := fmt.Sprintf("GET /api/v3/repos/adaiversen/v9-live/issues/1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer tok\r\n\r\n", addr)
	for i := 0; i < 3; i++ {
		if _, err := conn.Write([]byte(getReq)); err != nil {
			t.Fatalf("write GET %d failed: %v", i, err)
		}
		n, err = conn.Read(buf)
		if err != nil && err != io.EOF {
			t.Fatalf("read GET %d failed: %v", i, err)
		}
		if !bytes.Contains(buf[:n], []byte("HTTP/1.1 200 OK")) || !bytes.Contains(buf[:n], []byte(`"number":1`)) {
			t.Fatalf("unexpected GET %d response: %s", i, string(buf[:n]))
		}
	}

	// 3. Close() must cleanly stop Serve() without hanging
	srv.Close()
	srv.Close() // idempotent
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve() did not exit promptly after Close()")
	}
}
