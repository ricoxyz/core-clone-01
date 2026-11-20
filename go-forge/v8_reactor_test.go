package main

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestV8RecvAdvImmutableFrames(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v8-immut", "test", false)

	blob1 := []byte("hello v8")
	tree1 := buildTree([]TreeEntry{{Mode: "100644", Name: "f.txt", OID: hashObject(ObjBlob, blob1)}})
	tree1OID := hashObject(ObjTree, tree1)
	c1 := buildCommit(tree1OID, nil, "c1", "dev", 1700000001)
	c1OID := hashObject(ObjCommit, c1)
	pack1 := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjBlob, blob1},
		{ObjTree, tree1},
		{ObjCommit, c1},
	})
	push1 := appendPktLineStr(nil, ZeroOID+" "+c1OID+" refs/heads/main\x00report-status side-band-64k\n")
	push1 = appendPktFlush(push1)
	push1 = append(push1, pack1...)
	resp1 := handleGitReceivePackRepo(repo, push1, nil)
	if !bytes.Contains(resp1, []byte("ok refs/heads/main")) {
		t.Fatalf("push1 failed: %s", string(resp1))
	}

	adv1 := repo.recvAdvCache
	adv1Copy := append([]byte(nil), adv1...)
	if !bytes.Contains(adv1, []byte(c1OID)) {
		t.Fatalf("adv1 missing c1OID")
	}

	c2 := buildCommit(tree1OID, []string{c1OID}, "c2", "dev", 1700000002)
	c2OID := hashObject(ObjCommit, c2)
	pack2 := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjCommit, c2},
	})
	push2 := appendPktLineStr(nil, c1OID+" "+c2OID+" refs/heads/main\x00report-status side-band-64k\n")
	push2 = appendPktFlush(push2)
	push2 = append(push2, pack2...)
	resp2 := handleGitReceivePackRepo(repo, push2, nil)
	if !bytes.Contains(resp2, []byte("ok refs/heads/main")) {
		t.Fatalf("push2 failed: %s", string(resp2))
	}

	adv2 := repo.recvAdvCache
	if !bytes.Equal(adv1, adv1Copy) {
		t.Fatalf("adv1 was mutated in-place after push2; expected immutable response slice")
	}
	if !bytes.Contains(adv2, []byte(c2OID)) || bytes.Equal(adv1, adv2) {
		t.Fatalf("adv2 not updated properly")
	}
}

func TestV8ConcurrentStoreMuTwoReactors(t *testing.T) {
	store := NewForgeStore()
	store.CreateRepo("adaiversen", "v8-dual", "test", false)
	srv := NewServer(store)

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func(reactorID int) {
			defer wg.Done()
			c := newConnState()
			for i := 0; i < 40; i++ {
				body := fmt.Sprintf(`{"title":"r%d-%d","body":"concurrent test"}`, reactorID, i)
				raw := fmt.Sprintf(
					"POST /api/v3/repos/adaiversen/v8-dual/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer tok\r\nContent-Length: %d\r\n\r\n%s",
					len(body), body,
				)
				c.prepareReadWindow()
				copy(c.buf[c.end:], raw)
				c.end += len(raw)

				srv.fallbackMu.Lock()
				resp, needMore, _, closeAfter, fatal := srv.stepConn(c)
				out := append([]byte(nil), resp...)
				srv.fallbackMu.Unlock()

				if fatal || needMore || closeAfter || !bytes.Contains(out, []byte("201 Created")) {
					errs <- fmt.Errorf("reactor %d iter %d unexpected result: needMore=%v close=%v fatal=%v resp=%s",
						reactorID, i, needMore, closeAfter, fatal, string(out))
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	repo := store.GetRepo("adaiversen", "v8-dual")
	if len(repo.Issues) != 80 {
		t.Fatalf("expected 80 issues across 2 reactors, got %d", len(repo.Issues))
	}
	srv.Close()
	srv.Close()
}
