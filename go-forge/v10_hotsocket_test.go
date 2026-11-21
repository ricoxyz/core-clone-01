package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func readRawHTTPResponse(r *bufio.Reader) (string, []byte, error) {
	var hdrBuf bytes.Buffer
	contentLen := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		hdrBuf.WriteString(line)
		if line == "\r\n" {
			break
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "content-length:") {
			val := strings.TrimSpace(line[len("content-length:"):])
			_, _ = fmt.Sscanf(val, "%d", &contentLen)
		}
	}
	body := make([]byte, contentLen)
	if contentLen > 0 {
		if _, err := io.ReadFull(r, body); err != nil {
			return hdrBuf.String(), nil, err
		}
	}
	return hdrBuf.String(), body, nil
}

func TestV10HotSocketSequentialReadsAndWrites(t *testing.T) {
	store := NewForgeStore()
	store.CreateRepo("adaiversen", "v10-hot", "test", false)
	srv := NewServer(store)

	port, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
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
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	rd := bufio.NewReader(conn)

	// 200 sequential POST /issues on single keep-alive socket (triggers hotStreak >= 3)
	for i := 1; i <= 200; i++ {
		payload := fmt.Sprintf(`{"title":"hot-%d","body":"v10 burst"}`, i)
		req := fmt.Sprintf(
			"POST /api/v3/repos/adaiversen/v10-hot/issues HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer tok\r\nContent-Length: %d\r\n\r\n%s",
			addr, len(payload), payload,
		)
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write POST %d failed: %v", i, err)
		}
		hdr, body, err := readRawHTTPResponse(rd)
		if err != nil {
			t.Fatalf("read POST %d failed: %v", i, err)
		}
		if !strings.HasPrefix(hdr, "HTTP/1.1 201 Created") || !bytes.Contains(body, []byte(fmt.Sprintf(`"number":%d`, i))) {
			t.Fatalf("unexpected POST %d response: hdr=%q body=%q", i, hdr, string(body))
		}
	}

	// 200 sequential GET /issues/1 on same socket
	getReq := fmt.Sprintf("GET /api/v3/repos/adaiversen/v10-hot/issues/1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer tok\r\n\r\n", addr)
	for i := 1; i <= 200; i++ {
		if _, err := conn.Write([]byte(getReq)); err != nil {
			t.Fatalf("write GET %d failed: %v", i, err)
		}
		hdr, body, err := readRawHTTPResponse(rd)
		if err != nil {
			t.Fatalf("read GET %d failed: %v", i, err)
		}
		if !strings.HasPrefix(hdr, "HTTP/1.1 200 OK") || !bytes.Contains(body, []byte(`"title":"hot-1"`)) {
			t.Fatalf("unexpected GET %d response: hdr=%q body=%q", i, hdr, string(body))
		}
	}

	srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve() did not exit cleanly after Close()")
	}
}

func TestV10ConcurrentSecondConnectionAfterIdle(t *testing.T) {
	store := NewForgeStore()
	store.CreateRepo("adaiversen", "v10-multi", "test", false)
	srv := NewServer(store)

	port, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		srv.Serve()
		close(done)
	}()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c1, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		srv.Close()
		t.Fatalf("Dial c1 failed: %v", err)
	}
	defer c1.Close()
	_ = c1.SetDeadline(time.Now().Add(5 * time.Second))
	rd1 := bufio.NewReader(c1)

	// Drive c1 past hotStreak >= 3, then leave c1 open and idle
	healthReq := fmt.Sprintf("GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	for i := 0; i < 6; i++ {
		if _, err := c1.Write([]byte(healthReq)); err != nil {
			t.Fatalf("c1 write %d failed: %v", i, err)
		}
		hdr, _, err := readRawHTTPResponse(rd1)
		if err != nil || !strings.HasPrefix(hdr, "HTTP/1.1 200 OK") {
			t.Fatalf("c1 read %d failed: hdr=%q err=%v", i, hdr, err)
		}
	}

	// Wait past 5 ms SO_RCVTIMEO window so reactor returns to epoll_pwait
	time.Sleep(15 * time.Millisecond)

	// Second concurrent client must be served promptly while c1 remains open
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", addr))
	if err != nil {
		t.Fatalf("c2 GET /healthz failed while c1 idle: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 from c2, got %d", resp.StatusCode)
	}

	// c1 must still work cleanly over keep-alive afterwards
	if _, err := c1.Write([]byte(healthReq)); err != nil {
		t.Fatalf("c1 post-idle write failed: %v", err)
	}
	hdr, _, err := readRawHTTPResponse(rd1)
	if err != nil || !strings.HasPrefix(hdr, "HTTP/1.1 200 OK") {
		t.Fatalf("c1 post-idle read failed: hdr=%q err=%v", hdr, err)
	}

	srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Serve() did not exit cleanly after Close()")
	}
}

func TestV10Offset12CommitFastPathAndRecvAdvBuf(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v10-off12", "test", false)

	blob := []byte("v10 payload")
	treeData := buildTree([]TreeEntry{{Mode: "100644", Name: "f.txt", OID: hashObject(ObjBlob, blob)}})
	treeOID := hashObject(ObjTree, treeData)
	c1 := buildCommit(treeOID, nil, "c1", "dev", 1700000001)
	c1OID := hashObject(ObjCommit, c1)
	c2 := buildCommit(treeOID, []string{c1OID}, "c2", "dev", 1700000002)
	c2OID := hashObject(ObjCommit, c2)
	cBad := buildCommit(treeOID, []string{strings.Repeat("b", 40)}, "bad", "dev", 1700000003)
	cBadOID := hashObject(ObjCommit, cBad)

	// Place commit FIRST (at exact pack offset 12) before tree and blob
	packOffset12 := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjCommit, c2},
		{ObjTree, treeData},
		{ObjBlob, blob},
	})
	fastOK, parentOK, err := ingestPackFastForwardMain(packOffset12, repo, []byte(c1OID), []byte(c2OID))
	if err != nil || !fastOK || !parentOK {
		t.Fatalf("offset-12 fast path failed: fastOK=%v parentOK=%v err=%v", fastOK, parentOK, err)
	}

	// Non-FF commit at offset 12 must be rejected cleanly
	packNonFF12 := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjCommit, cBad},
		{ObjBlob, blob},
	})
	fastOK, parentOK, err = ingestPackFastForwardMain(packNonFF12, repo, []byte(c1OID), []byte(cBadOID))
	if err != nil || fastOK || parentOK {
		t.Fatalf("expected offset-12 non-FF rejection, got fastOK=%v parentOK=%v err=%v", fastOK, parentOK, err)
	}

	// Verify recvAdvBuf in-place update across sequential main pushes
	pack1 := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjBlob, blob},
		{ObjTree, treeData},
		{ObjCommit, c1},
	})
	push1 := appendPktLineStr(nil, ZeroOID+" "+c1OID+" refs/heads/main\x00report-status side-band-64k\n")
	push1 = appendPktFlush(push1)
	push1 = append(push1, pack1...)
	if resp := handleGitReceivePackRepo(repo, push1, nil); !bytes.Contains(resp, []byte("ok refs/heads/main")) {
		t.Fatalf("push1 failed: %s", string(resp))
	}
	if !bytes.Contains(repo.recvAdvCache, []byte(c1OID)) || len(repo.recvAdvBuf) == 0 {
		t.Fatalf("recvAdvBuf not initialized after push1")
	}

	push2 := appendPktLineStr(nil, c1OID+" "+c2OID+" refs/heads/main\x00report-status side-band-64k\n")
	push2 = appendPktFlush(push2)
	push2 = append(push2, packOffset12...)
	if resp := handleGitReceivePackRepo(repo, push2, nil); !bytes.Contains(resp, []byte("ok refs/heads/main")) {
		t.Fatalf("push2 failed: %s", string(resp))
	}
	if !bytes.Contains(repo.recvAdvCache, []byte(c2OID)) {
		t.Fatalf("recvAdvCache missing c2OID after push2")
	}
}
