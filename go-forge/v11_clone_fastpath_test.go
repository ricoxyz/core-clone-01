package main

import (
	"bytes"
	"testing"
)

func buildPktBody(lines ...string) []byte {
	var out []byte
	for _, l := range lines {
		switch l {
		case "flush":
			out = appendPktFlush(out)
		case "delim":
			out = appendPktDelim(out)
		default:
			out = appendPktLineStr(out, l)
		}
	}
	return out
}

func TestV11CloneFastPathLsRefsHeadsAndOutOfOrderFetch(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v11-clone", "test", false)

	blob := []byte("hello v11")
	treeData := buildTree([]TreeEntry{{Mode: "100644", Name: "README.md", OID: hashObject(ObjBlob, blob)}})
	treeOID := hashObject(ObjTree, treeData)
	c1 := buildCommit(treeOID, nil, "init", "dev", 1700000001)
	c1OID := hashObject(ObjCommit, c1)
	c2 := buildCommit(treeOID, []string{c1OID}, "tip", "dev", 1700000002)
	c2OID := hashObject(ObjCommit, c2)

	pack := buildTestPack([]struct {
		typ  GitObjType
		data []byte
	}{
		{ObjBlob, blob},
		{ObjTree, treeData},
		{ObjCommit, c1},
		{ObjCommit, c2},
	})
	if err := readPackIntoRepo(pack, repo); err != nil {
		t.Fatalf("readPackIntoRepo failed: %v", err)
	}
	repo.Refs["refs/heads/main"] = c2OID
	copy(repo.mainTip[:], c2OID)
	repo.hasMainTip = true

	// Warm lsRefsHeadsCache and lsRefsAllCache + cloneDepth1Cache
	_ = handleGitUploadPackFastRepo(repo, buildPktBody("command=ls-refs\n", "delim", "peel\n", "symrefs\n", "flush"))
	_ = handleGitUploadPackFastRepo(repo, buildPktBody("command=ls-refs\n", "delim", "peel\n", "symrefs\n", "ref-prefix HEAD\n", "ref-prefix refs/heads/\n", "flush"))
	_ = handleGitUploadPackFastRepo(repo, buildPktBody("command=fetch\n", "delim", "thin-pack\n", "no-progress\n", "ofs-delta\n", "want "+c2OID+"\n", "deepen 1\n", "done\n", "flush"))

	if len(repo.lsRefsAllCache) == 0 || len(repo.lsRefsHeadsCache) == 0 || repo.cloneDepth1Cache == nil {
		t.Fatalf("expected all v11 clone caches populated")
	}

	// Verify ls-refs with ref-prefix HEAD + ref-prefix refs/heads/ in BOTH orders hits exact lsRefsHeadsCache pointer
	headsFwd := handleGitUploadPackFastRepo(repo, buildPktBody("command=ls-refs\n", "agent=test\n", "delim", "peel\n", "symrefs\n", "ref-prefix HEAD\n", "ref-prefix refs/heads/\n", "flush"))
	if len(headsFwd) != len(repo.lsRefsHeadsCache) || &headsFwd[0] != &repo.lsRefsHeadsCache[0] {
		t.Fatalf("expected lsRefsHeadsCache direct hit on forward ref-prefix order")
	}
	headsRev := handleGitUploadPackFastRepo(repo, buildPktBody("command=ls-refs\n", "delim", "symrefs\n", "ref-prefix refs/heads/\n", "ref-prefix HEAD\n", "peel\n", "flush"))
	if len(headsRev) != len(repo.lsRefsHeadsCache) || &headsRev[0] != &repo.lsRefsHeadsCache[0] {
		t.Fatalf("expected lsRefsHeadsCache direct hit on reversed ref-prefix order")
	}

	// Verify out-of-order fetch pkt-lines hit exact cloneDepth1Cache.httpResp pointer
	reorderedFetch := handleGitUploadPackFastRepo(repo, buildPktBody(
		"command=fetch\n",
		"agent=makefun-git/1.0\n",
		"delim",
		"deepen 1\n",
		"ofs-delta\n",
		"want "+c2OID+"\n",
		"done\n",
		"thin-pack\n",
		"no-progress\n",
		"flush",
	))
	if len(reorderedFetch) != len(repo.cloneDepth1Cache.httpResp) || &reorderedFetch[0] != &repo.cloneDepth1Cache.httpResp[0] {
		t.Fatalf("expected cloneDepth1Cache direct hit on out-of-order fetch pkt-lines")
	}

	// Verify deepen 2 does NOT erroneously hit cloneDepth1Cache (must fall back and include c1OID shallow/pack)
	deepen2 := handleGitUploadPackFastRepo(repo, buildPktBody(
		"command=fetch\n",
		"delim",
		"want "+c2OID+"\n",
		"deepen 2\n",
		"done\n",
		"flush",
	))
	if &deepen2[0] == &repo.cloneDepth1Cache.httpResp[0] {
		t.Fatalf("deepen 2 unexpectedly hit depth-1 cache")
	}
	if !bytes.Contains(deepen2, []byte("packfile\n")) {
		t.Fatalf("deepen 2 fallback failed to return packfile")
	}
}
