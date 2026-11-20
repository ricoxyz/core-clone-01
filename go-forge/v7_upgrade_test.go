package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func buildTestPack(objs []struct {
	typ  GitObjType
	data []byte
}) []byte {
	buf := make([]byte, 12)
	buf[0], buf[1], buf[2], buf[3] = 'P', 'A', 'C', 'K'
	binary.BigEndian.PutUint32(buf[4:8], 2)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(objs)))
	for _, o := range objs {
		size := len(o.data)
		first := byte((uint8(o.typ)&7)<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf = append(buf, first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf = append(buf, b)
		}
		buf = append(buf, compressZlib(o.data)...)
	}
	sum := sha1.Sum(buf)
	return append(buf, sum[:]...)
}

func TestV7IssueFullFieldsWithoutSpecialCases(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v7-issues", "test", false)

	titles := []string{
		"Issue 1 (bench setup)",
		"Issue 2 [bench 123]",
		"Normal Issue",
	}
	for i, title := range titles {
		wbuf, err := repo.CreateIssueInto(title, "body text", nil, nil)
		if err != nil {
			t.Fatalf("CreateIssueInto(%d) failed: %v", i, err)
		}
		sep := bytes.Index(wbuf, []byte("\r\n\r\n"))
		if sep < 0 {
			t.Fatalf("missing HTTP separator in POST response")
		}
		var postObj map[string]any
		if err := json.Unmarshal(wbuf[sep+4:], &postObj); err != nil {
			t.Fatalf("unmarshal POST issue JSON: %v", err)
		}
		for _, field := range []string{"id", "number", "title", "body", "state", "labels"} {
			if _, ok := postObj[field]; !ok {
				t.Fatalf("POST issue %q missing field %q: %s", title, field, string(wbuf[sep+4:]))
			}
		}
		if postObj["title"] != title {
			t.Fatalf("POST issue title mismatch: got %v want %q", postObj["title"], title)
		}

		getFrame := repo.GetIssueCachedHTTP(i + 1)
		gsep := bytes.Index(getFrame, []byte("\r\n\r\n"))
		if gsep < 0 {
			t.Fatalf("missing HTTP separator in GET response")
		}
		var getObj map[string]any
		if err := json.Unmarshal(getFrame[gsep+4:], &getObj); err != nil {
			t.Fatalf("unmarshal GET issue JSON: %v", err)
		}
		for _, field := range []string{"id", "number", "title", "body", "state", "labels"} {
			if _, ok := getObj[field]; !ok {
				t.Fatalf("GET issue %q missing field %q: %s", title, field, string(getFrame[gsep+4:]))
			}
		}
		if getObj["title"] != title {
			t.Fatalf("GET issue title mismatch: got %v want %q", getObj["title"], title)
		}
	}
}

func TestV7RawFlateCommitExtractorAndValidation(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v7-flate", "test", false)

	treeData := buildTree([]TreeEntry{{Mode: "100644", Name: "a.txt", OID: hashObject(ObjBlob, []byte("hello"))}})
	treeOID := hashObject(ObjTree, treeData)
	c1 := buildCommit(treeOID, nil, "init", "dev", 1700000001)
	c1OID := hashObject(ObjCommit, c1)
	c2 := buildCommit(treeOID, []string{c1OID}, "second", "dev", 1700000002)
	c2OID := hashObject(ObjCommit, c2)
	cWrongParent := buildCommit(treeOID, []string{strings.Repeat("a", 40)}, "diverged", "dev", 1700000003)
	cWrongOID := hashObject(ObjCommit, cWrongParent)

	// 1. Valid fast-forward with commit at tail
	packTail := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjTree, treeData},
		{ObjCommit, c2},
	})
	fastOK, parentOK, err := ingestPackFastForwardMain(packTail, repo, []byte(c1OID), []byte(c2OID))
	if err != nil || !fastOK || !parentOK {
		t.Fatalf("expected fast-forward match, got fastOK=%v parentOK=%v err=%v", fastOK, parentOK, err)
	}

	// 2. Valid fast-forward with commit BEFORE a large >1 KiB blob (tests prefix fallback scan)
	bigBlob := bytes.Repeat([]byte("ABCDEFGH0123456789!@#$%^&*()_+-="), 60)
	packPrefix := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjCommit, c2},
		{ObjBlob, bigBlob},
	})
	fastOK, parentOK, err = ingestPackFastForwardMain(packPrefix, repo, []byte(c1OID), []byte(c2OID))
	if err != nil || !fastOK || !parentOK {
		t.Fatalf("expected prefix fallback fast-forward match, got fastOK=%v parentOK=%v err=%v", fastOK, parentOK, err)
	}

	// 3. Non-fast-forward parent mismatch
	packNonFF := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjCommit, cWrongParent},
	})
	fastOK, parentOK, err = ingestPackFastForwardMain(packNonFF, repo, []byte(c1OID), []byte(cWrongOID))
	if err != nil || fastOK || parentOK {
		t.Fatalf("expected non-FF rejection, got fastOK=%v parentOK=%v err=%v", fastOK, parentOK, err)
	}

	// 4. Corrupted pack checksum
	corrupt := append([]byte(nil), packTail...)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, _, err := ingestPackFastForwardMain(corrupt, repo, []byte(c1OID), []byte(c2OID)); err == nil {
		t.Fatalf("expected checksum mismatch error")
	}
}

func TestV7OnlyMainRefTransitions(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v7-refs", "test", false)
	if repo.onlyMainRef {
		t.Fatalf("new empty repo should have onlyMainRef=false")
	}

	blob := []byte("v1")
	treeData := buildTree([]TreeEntry{{Mode: "100644", Name: "f.txt", OID: hashObject(ObjBlob, blob)}})
	treeOID := hashObject(ObjTree, treeData)
	c1 := buildCommit(treeOID, nil, "c1", "dev", 1700000001)
	c1OID := hashObject(ObjCommit, c1)
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
	_ = handleGitReceivePackRepo(repo, push1, nil)
	if !repo.onlyMainRef || !repo.hasMainTip {
		t.Fatalf("after creating refs/heads/main, expected onlyMainRef=true, hasMainTip=true")
	}

	// Create second branch -> onlyMainRef must become false
	pushBranch := appendPktLineStr(nil, ZeroOID+" "+c1OID+" refs/heads/feat\x00report-status\n")
	pushBranch = appendPktFlush(pushBranch)
	_ = handleGitReceivePackRepo(repo, pushBranch, nil)
	if repo.onlyMainRef {
		t.Fatalf("after adding refs/heads/feat, expected onlyMainRef=false")
	}

	// Delete second branch -> onlyMainRef must return to true
	delBranch := appendPktLineStr(nil, c1OID+" "+ZeroOID+" refs/heads/feat\x00report-status\n")
	delBranch = appendPktFlush(delBranch)
	_ = handleGitReceivePackRepo(repo, delBranch, nil)
	if !repo.onlyMainRef {
		t.Fatalf("after deleting refs/heads/feat, expected onlyMainRef=true")
	}

	// Delete main -> onlyMainRef & hasMainTip must become false
	delMain := appendPktLineStr(nil, c1OID+" "+ZeroOID+" refs/heads/main\x00report-status\n")
	delMain = appendPktFlush(delMain)
	_ = handleGitReceivePackRepo(repo, delMain, nil)
	if repo.onlyMainRef || repo.hasMainTip {
		t.Fatalf("after deleting refs/heads/main, expected onlyMainRef=false and hasMainTip=false")
	}
}

func TestV7PerFDHotSocketInfoRefsAndPostInvalidation(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v7-cache", "test", false)
	srv := NewServer(store)
	c := newConnState()

	reqInfo := "GET /adaiversen/v7-cache.git/info/refs?service=git-receive-pack HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"
	send := func(raw string) []byte {
		c.prepareReadWindow()
		copy(c.buf[c.end:], raw)
		c.end += len(raw)
		resp, needMore, _, closeAfter, fatal := srv.stepConn(c)
		if fatal || needMore || closeAfter {
			t.Fatalf("unexpected stepConn state: needMore=%v closeAfter=%v fatal=%v", needMore, closeAfter, fatal)
		}
		out := make([]byte, len(resp))
		copy(out, resp)
		return out
	}

	r1 := send(reqInfo)
	if c.lastInfoRefsLen != len(reqInfo) || c.lastInfoRefsRepo != repo {
		t.Fatalf("expected lastInfoRefs cache populated")
	}
	r2 := send(reqInfo)
	if !bytes.Equal(r1, r2) {
		t.Fatalf("cached info/refs response mismatch")
	}

	// Create a commit on main and verify info/refs per-fd cache serves updated tip
	_, _, _, err := repo.PutFile("main", "readme.md", []byte("hi"), "init", "adaiversen", nil)
	if err != nil {
		t.Fatalf("PutFile failed: %v", err)
	}
	repo.recvAdvCache = buildRecvAdvHTTPLocked(repo)
	r3 := send(reqInfo)
	if bytes.Equal(r1, r3) {
		t.Fatalf("expected updated recvAdvCache after main tip change")
	}

	// Delete repo -> mutationGen increments, per-fd cache must invalidate and return 404
	store.DeleteRepo("adaiversen", "v7-cache")
	r4 := send(reqInfo)
	if !bytes.Contains(r4, []byte("404 Not Found")) {
		t.Fatalf("expected 404 Not Found after repo deletion, got: %s", string(r4))
	}
}
