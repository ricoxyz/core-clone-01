package main

import (
	"bytes"
	"strings"
	"testing"
)

func feedAndStepAll(t *testing.T, srv *Server, c *connState, chunks ...[]byte) [][]byte {
	t.Helper()
	var responses [][]byte
	for _, ch := range chunks {
		c.prepareReadWindow()
		if len(c.buf)-c.end < len(ch) {
			nb := make([]byte, c.end+len(ch)+4096)
			copy(nb, c.buf[:c.end])
			c.buf = nb
		}
		copy(c.buf[c.end:], ch)
		c.end += len(ch)
		for c.end > c.start {
			resp, needMore, _, _, fatal := srv.stepConn(c, nil)
			if fatal {
				t.Fatalf("unexpected fatal parser error")
			}
			if len(resp) > 0 {
				cp := make([]byte, len(resp))
				copy(cp, resp)
				responses = append(responses, cp)
			}
			if needMore {
				break
			}
		}
	}
	return responses
}

func TestStepConnSplitHeadersAndBodyAndPerFDCacheInvalidation(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	store.CreateRepo("adaiversen", "bench-read", "test", false)

	c := newConnState()
	// 1. Create an issue with split header and split body across 4 tiny reads
	postReq := "POST /api/v3/repos/adaiversen/bench-read/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\nContent-Length: 35\r\n\r\n{\"title\":\"first\",\"body\":\"hello-v6\"}"
	resps := feedAndStepAll(t, srv, c,
		[]byte(postReq[:25]),
		[]byte(postReq[25:80]),
		[]byte(postReq[80:len(postReq)-10]),
		[]byte(postReq[len(postReq)-10:]),
	)
	if len(resps) != 1 || !bytes.Contains(resps[0], []byte("201 Created")) {
		t.Fatalf("expected 1 201 Created response, got %d (%s)", len(resps), string(bytes.Join(resps, nil)))
	}

	// 2. Sequential identical GETs on the same connState: first warms per-FD cache, second hits byte-compare fast path
	getReq := []byte("GET /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\n\r\n")
	resps = feedAndStepAll(t, srv, c, getReq, getReq)
	if len(resps) != 2 {
		t.Fatalf("expected 2 GET responses, got %d", len(resps))
	}
	if !bytes.Equal(resps[0], resps[1]) || !bytes.Contains(resps[0], []byte(`"title":"first"`)) {
		t.Fatalf("unexpected GET response: %s", string(resps[0]))
	}
	if c.lastGetLen != len(getReq) {
		t.Fatalf("expected per-fd lastGetLen=%d, got %d", len(getReq), c.lastGetLen)
	}

	// 3. Pipelined two GETs in a single read packet
	pipelined := append(append([]byte(nil), getReq...), getReq...)
	resps = feedAndStepAll(t, srv, c, pipelined)
	if len(resps) != 2 || !bytes.Equal(resps[0], resps[1]) {
		t.Fatalf("expected 2 identical pipelined GET responses, got %d", len(resps))
	}

	// 4. PATCH issue #1 on another connection and verify per-FD cache on connection c immediately invalidates via issueGen!
	c2 := newConnState()
	patchBody := `{"state":"closed","title":"updated-title"}`
	patchReq := "PATCH /api/v3/repos/adaiversen/bench-read/issues/1 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\nContent-Length: 42\r\n\r\n" + patchBody
	resps2 := feedAndStepAll(t, srv, c2, []byte(patchReq))
	if len(resps2) != 1 || !bytes.Contains(resps2[0], []byte("200 OK")) {
		t.Fatalf("PATCH failed: %v", resps2)
	}

	respsAfter := feedAndStepAll(t, srv, c, getReq)
	if len(respsAfter) != 1 || !bytes.Contains(respsAfter[0], []byte(`"title":"updated-title"`)) || !bytes.Contains(respsAfter[0], []byte(`"state":"closed"`)) {
		t.Fatalf("per-FD cache did not invalidate after PATCH: %s", string(bytes.Join(respsAfter, nil)))
	}
}

func TestStepConnChunkedAndLargeBufferGrowth(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	store.CreateRepo("adaiversen", "chunk-repo", "test", false)

	c := newConnState()
	// Chunked request delivered across 3 separate socket reads
	chunkHdr := "POST /api/v3/repos/adaiversen/chunk-repo/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\nTransfer-Encoding: chunked\r\n\r\n"
	chunk1 := "10\r\n{\"title\":\"chunke\r\n"
	chunk2 := "12\r\nd\",\"body\":\"world\"}\r\n0\r\n\r\n"

	resps := feedAndStepAll(t, srv, c, []byte(chunkHdr), []byte(chunk1), []byte(chunk2))
	if len(resps) != 1 || !bytes.Contains(resps[0], []byte("201 Created")) || !bytes.Contains(resps[0], []byte(`"title":"chunked"`)) {
		t.Fatalf("chunked POST failed: %s", string(bytes.Join(resps, nil)))
	}

	// Large payload (> 64 KiB) to verify dynamic buffer growth from 4 KiB initial buffer
	bigBody := strings.Repeat("A", 70000)
	jsonPayload := `{"title":"big","body":"` + bigBody + `"}`
	bigReq := "POST /api/v3/repos/adaiversen/chunk-repo/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\nContent-Length: " +
		"70025\r\n\r\n" + jsonPayload
	resps = feedAndStepAll(t, srv, c, []byte(bigReq[:1024]), []byte(bigReq[1024:]))
	if len(resps) != 1 || !bytes.Contains(resps[0], []byte("201 Created")) {
		t.Fatalf("large payload POST failed")
	}
}

func TestZeroAllocPushAndLevel9CloneCache(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "git-v6", "test", false)

	blob1 := []byte("hello world v6\n")
	blob1OID := hashObject(ObjBlob, blob1)
	tree1 := buildTree([]TreeEntry{{Mode: "100644", Name: "README.md", OID: blob1OID}})
	tree1OID := hashObject(ObjTree, tree1)
	commit1 := buildCommit(tree1OID, nil, "init\n", "Tester <t@make.fun>", 1700000000)
	commit1OID := hashObject(ObjCommit, commit1)

	pack1 := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(blob1), Data: blob1},
		{Type: ObjTree, Size: len(tree1), Data: tree1},
		{Type: ObjCommit, Size: len(commit1), Data: commit1},
	})

	var push1 []byte
	push1 = appendPktLineStr(push1, ZeroOID+" "+commit1OID+" refs/heads/main\x00report-status side-band-64k ofs-delta\n")
	push1 = appendPktFlush(push1)
	push1 = append(push1, pack1...)

	resp1 := handleGitReceivePackRepo(repo, push1, nil)
	if !bytes.Contains(resp1, []byte("ok refs/heads/main")) {
		t.Fatalf("initial push failed: %s", string(resp1))
	}
	if !repo.hasMainTip || string(repo.mainTip[:]) != commit1OID {
		t.Fatalf("mainTip not updated to %s", commit1OID)
	}

	// Second fast-forward push on refs/heads/main
	blob2 := []byte("hello world v6 step 2\n")
	blob2OID := hashObject(ObjBlob, blob2)
	tree2 := buildTree([]TreeEntry{{Mode: "100644", Name: "README.md", OID: blob2OID}})
	tree2OID := hashObject(ObjTree, tree2)
	commit2 := buildCommit(tree2OID, []string{commit1OID}, "step 2\n", "Tester <t@make.fun>", 1700000001)
	commit2OID := hashObject(ObjCommit, commit2)

	pack2 := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(blob2), Data: blob2},
		{Type: ObjTree, Size: len(tree2), Data: tree2},
		{Type: ObjCommit, Size: len(commit2), Data: commit2},
	})

	var push2 []byte
	push2 = appendPktLineStr(push2, commit1OID+" "+commit2OID+" refs/heads/main\x00report-status side-band-64k ofs-delta\n")
	push2 = appendPktFlush(push2)
	push2 = append(push2, pack2...)

	resp2 := handleGitReceivePackRepo(repo, push2, nil)
	if !bytes.Contains(resp2, []byte("ok refs/heads/main")) {
		t.Fatalf("fast-forward push failed: %s", string(resp2))
	}
	if string(repo.mainTip[:]) != commit2OID {
		t.Fatalf("mainTip not advanced to %s", commit2OID)
	}

	// Non-fast-forward push (divergent root commit claiming oldOID=commit2OID) must be rejected
	commitBad := buildCommit(tree1OID, nil, "unrelated root\n", "Tester <t@make.fun>", 1700000002)
	commitBadOID := hashObject(ObjCommit, commitBad)
	packBad := writePack([]*GitObject{
		{Type: ObjCommit, Size: len(commitBad), Data: commitBad},
	})
	var pushBad []byte
	pushBad = appendPktLineStr(pushBad, commit2OID+" "+commitBadOID+" refs/heads/main\x00report-status side-band-64k ofs-delta\n")
	pushBad = appendPktFlush(pushBad)
	pushBad = append(pushBad, packBad...)

	respBad := handleGitReceivePackRepo(repo, pushBad, nil)
	if !bytes.Contains(respBad, []byte("ng refs/heads/main non-fast-forward")) {
		t.Fatalf("expected non-fast-forward rejection, got: %s", string(respBad))
	}

	// Protocol v2 depth-1 fetch warms cloneDepth1Cache and hits zero-scan fast path on repeat call
	var fetchReq []byte
	fetchReq = appendPktLineStr(fetchReq, "command=fetch\n")
	fetchReq = appendPktDelim(fetchReq)
	fetchReq = appendPktLineStr(fetchReq, "thin-pack\n")
	fetchReq = appendPktLineStr(fetchReq, "no-progress\n")
	fetchReq = appendPktLineStr(fetchReq, "ofs-delta\n")
	fetchReq = appendPktLineStr(fetchReq, "want "+commit2OID+"\n")
	fetchReq = appendPktLineStr(fetchReq, "deepen 1\n")
	fetchReq = appendPktLineStr(fetchReq, "done\n")
	fetchReq = appendPktFlush(fetchReq)

	cloneResp1 := handleGitUploadPackFastRepo(repo, fetchReq)
	if repo.cloneDepth1Cache == nil {
		t.Fatalf("expected cloneDepth1Cache to be populated")
	}
	cloneResp2 := handleGitUploadPackFastRepo(repo, fetchReq)
	if !bytes.Equal(cloneResp1, cloneResp2) {
		t.Fatalf("cached clone response mismatch")
	}
}
