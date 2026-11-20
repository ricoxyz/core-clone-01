package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestV6GitProtocolV2AdvertisementAndInfoRefs(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	store.CreateRepo("adaiversen", "proto-v6-repo", "v6 protocol test", true)

	conn := newConnState()
	reqMissing := "GET /adaiversen/no-such-repo-v6.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: 127.0.0.1\r\nGit-Protocol: version=2\r\n\r\n"
	outMissing := feedAndStepAll(t, srv, conn, []byte(reqMissing))
	if len(outMissing) != 1 || !bytes.Contains(outMissing[0], []byte("404 Not Found")) {
		t.Fatalf("v6 missing repo info/refs did not return 404: %v", outMissing)
	}

	reqUploadV2 := "GET /adaiversen/proto-v6-repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: 127.0.0.1\r\nGit-Protocol: version=2\r\n\r\n"
	outUploadV2 := feedAndStepAll(t, srv, conn, []byte(reqUploadV2))
	if len(outUploadV2) != 1 || !bytes.Contains(outUploadV2[0], []byte("version 2\n")) {
		t.Fatalf("v6 upload-pack v2 advertisement missing version 2 preamble")
	}

	reqUploadV0 := "GET /adaiversen/proto-v6-repo.git/info/refs?service=git-upload-pack HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"
	outUploadV0 := feedAndStepAll(t, srv, conn, []byte(reqUploadV0))
	if len(outUploadV0) != 1 || !bytes.Contains(outUploadV0[0], []byte("# service=git-upload-pack\n")) {
		t.Fatalf("v6 upload-pack v0 advertisement missing service preamble")
	}

	reqRecvV0 := "GET /adaiversen/proto-v6-repo.git/info/refs?service=git-receive-pack HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"
	outRecvV0 := feedAndStepAll(t, srv, conn, []byte(reqRecvV0))
	if len(outRecvV0) != 1 || !bytes.Contains(outRecvV0[0], []byte("# service=git-receive-pack\n")) {
		t.Fatalf("v6 receive-pack v0 advertisement missing service preamble")
	}
}

func TestV6GitMultiBranchPushAndAnnotatedTagPeeling(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "tags-v6-repo", "v6 tag and branch test", false)

	bData := []byte("file content for v6 tag verification\n")
	bOID := hashObject(ObjBlob, bData)
	tData := buildTree([]TreeEntry{{Mode: "100644", Name: "app.go", OID: bOID}})
	tOID := hashObject(ObjTree, tData)
	cData := buildCommit(tOID, nil, "v6 initial release\n", "V6 Author <v6@make.fun>", 1700000100)
	cOID := hashObject(ObjCommit, cData)

	tagRaw := []byte("object " + cOID + "\ntype commit\ntag v6.0.0\ntagger V6 Author <v6@make.fun> 1700000100 +0000\n\nAnnotated v6 tag\n")
	tagOID := hashObject(ObjTag, tagRaw)

	packPayload := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(bData), Data: bData},
		{Type: ObjTree, Size: len(tData), Data: tData},
		{Type: ObjCommit, Size: len(cData), Data: cData},
		{Type: ObjTag, Size: len(tagRaw), Data: tagRaw},
	})

	var pushStream []byte
	pushStream = appendPktLineStr(pushStream, ZeroOID+" "+cOID+" refs/heads/main\x00report-status side-band-64k ofs-delta\n")
	pushStream = appendPktLineStr(pushStream, ZeroOID+" "+cOID+" refs/heads/feat-v6\n")
	pushStream = appendPktLineStr(pushStream, ZeroOID+" "+tagOID+" refs/tags/v6.0.0\n")
	pushStream = appendPktFlush(pushStream)
	pushStream = append(pushStream, packPayload...)

	pushReply := handleGitReceivePackRepo(repo, pushStream, nil)
	if !bytes.Contains(pushReply, []byte("ok refs/heads/main")) ||
		!bytes.Contains(pushReply, []byte("ok refs/heads/feat-v6")) ||
		!bytes.Contains(pushReply, []byte("ok refs/tags/v6.0.0")) {
		t.Fatalf("v6 multi-ref push failed: %s", string(pushReply))
	}

	var lsAllReq []byte
	lsAllReq = appendPktLineStr(lsAllReq, "command=ls-refs\n")
	lsAllReq = appendPktDelim(lsAllReq)
	lsAllReq = appendPktLineStr(lsAllReq, "peel\n")
	lsAllReq = appendPktLineStr(lsAllReq, "symrefs\n")
	lsAllReq = appendPktFlush(lsAllReq)

	lsAllResp := handleGitUploadPackFastRepo(repo, lsAllReq)
	if !bytes.Contains(lsAllResp, []byte("peeled:"+cOID)) {
		t.Fatalf("v6 ls-refs did not peel annotated tag to %s: %s", cOID, string(lsAllResp))
	}
	if !bytes.Contains(lsAllResp, []byte("symref-target:refs/heads/main")) {
		t.Fatalf("v6 ls-refs missing symref-target:refs/heads/main")
	}

	var lsPrefixReq []byte
	lsPrefixReq = appendPktLineStr(lsPrefixReq, "command=ls-refs\n")
	lsPrefixReq = appendPktDelim(lsPrefixReq)
	lsPrefixReq = appendPktLineStr(lsPrefixReq, "peel\n")
	lsPrefixReq = appendPktLineStr(lsPrefixReq, "symrefs\n")
	lsPrefixReq = appendPktLineStr(lsPrefixReq, "ref-prefix refs/tags/\n")
	lsPrefixReq = appendPktFlush(lsPrefixReq)

	lsPrefixResp := handleGitUploadPackFastRepo(repo, lsPrefixReq)
	if !bytes.Contains(lsPrefixResp, []byte("refs/tags/v6.0.0")) || bytes.Contains(lsPrefixResp, []byte("refs/heads/feat-v6")) {
		t.Fatalf("v6 ls-refs prefix filtering failed: %s", string(lsPrefixResp))
	}

	// Delete feature branch via receive-pack
	var delStream []byte
	delStream = appendPktLineStr(delStream, cOID+" "+ZeroOID+" refs/heads/feat-v6\x00report-status side-band-64k delete-refs\n")
	delStream = appendPktFlush(delStream)
	delReply := handleGitReceivePackRepo(repo, delStream, nil)
	if !bytes.Contains(delReply, []byte("ok refs/heads/feat-v6")) {
		t.Fatalf("v6 branch delete failed: %s", string(delReply))
	}
}

func TestV6GitDeltaPackResolutionAndCorruptionRejection(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "delta-v6-repo", "v6 delta test", false)

	baseBlob := []byte("0123456789abcdef0123456789abcdef\n")
	if len(hashObject(ObjBlob, baseBlob)) != 40 {
		t.Fatalf("v6 hashObject failed")
	}

	// Build a valid Git delta that copies 32 bytes from offset 0 of baseBlob and appends "v6-tail\n"
	var deltaInstr []byte
	deltaInstr = append(deltaInstr, byte(len(baseBlob))) // srcSize = 33
	deltaInstr = append(deltaInstr, byte(32+8))          // dstSize = 40
	deltaInstr = append(deltaInstr, 0x80|0x10, 32)       // copy offset=0, size=32
	deltaInstr = append(deltaInstr, 8)                   // insert 8 literal bytes
	deltaInstr = append(deltaInstr, "v6-tail\n"...)

	reconstructed, derr := applyDelta(baseBlob, deltaInstr)
	if derr != nil || string(reconstructed) != "0123456789abcdef0123456789abcdefv6-tail\n" {
		t.Fatalf("v6 applyDelta failed: %v (%q)", derr, string(reconstructed))
	}

	// Verify corrupted pack checksum rejection
	badPack := make([]byte, 32)
	copy(badPack[:4], "PACK")
	binary.BigEndian.PutUint32(badPack[4:8], 2)
	binary.BigEndian.PutUint32(badPack[8:12], 0)
	// Intentionally leave trailer as all zeroes (wrong SHA-1)
	if err := readPackIntoRepo(badPack, repo); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("v6 readPackIntoRepo should reject bad checksum, got: %v", err)
	}
}

func TestV6RestContentsPullsLabelsCommentsAndMerges(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	conn := newConnState()

	createRepoReq := "POST /api/v3/user/repos HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 58\r\n\r\n{\"name\":\"rest-v6\",\"description\":\"v6 suite\",\"private\":true}"
	res := feedAndStepAll(t, srv, conn, []byte(createRepoReq))
	if len(res) != 1 || !bytes.Contains(res[0], []byte("201 Created")) {
		t.Fatalf("v6 REST repo create failed: %s", string(bytes.Join(res, nil)))
	}

	repo := store.GetRepo("adaiversen", "rest-v6")
	if repo == nil {
		t.Fatalf("v6 repo not found in store")
	}

	// Put file on main branch and second file on feature branch
	mainSha, blobSha, isUpd, err := repo.PutFile("main", "pkg/core.txt", []byte("v6-main-line\n"), "add core.txt", "adaiversen", nil)
	if err != nil || isUpd || len(mainSha) != 40 || len(blobSha) != 40 {
		t.Fatalf("v6 PutFile main failed: %v", err)
	}
	repo.Refs["refs/heads/feature-v6"] = mainSha
	featSha, _, _, err := repo.PutFile("feature-v6", "pkg/feat.txt", []byte("v6-feature-line\n"), "add feat.txt", "adaiversen", nil)
	if err != nil || len(featSha) != 40 {
		t.Fatalf("v6 PutFile feature-v6 failed: %v", err)
	}

	// Verify directory listing and file retrieval
	isFile, contentBytes, gotSha, _, found := repo.GetFileOrDir("main", "pkg/core.txt")
	if !found || !isFile || gotSha != blobSha || string(contentBytes) != "v6-main-line\n" {
		t.Fatalf("v6 GetFileOrDir file mismatch")
	}
	isDirFile, _, _, dirList, dirFound := repo.GetFileOrDir("feature-v6", "pkg")
	if !dirFound || isDirFile || len(dirList) != 2 {
		t.Fatalf("v6 GetFileOrDir directory expected 2 entries, got %d", len(dirList))
	}

	// Verify stale SHA conflict check
	wrongSha := "1111111111111111111111111111111111111111"
	if _, _, _, serr := repo.PutFile("main", "pkg/core.txt", []byte("conflict"), "msg", "adaiversen", &wrongSha); serr == nil {
		t.Fatalf("v6 PutFile should reject stale SHA")
	}

	// Create label, issue with label, comment, pull request, and merge PR
	lbl := repo.CreateLabel("perf-v6", "00ff00", "v6 performance label")
	if lbl.Name != "perf-v6" || len(repo.ListLabels()) != 1 {
		t.Fatalf("v6 label creation failed")
	}
	iss, _, ierr := repo.CreateIssue("v6 latency target", "sub-325ms epoll", []string{"perf-v6"})
	if ierr != nil || iss.Number != 1 {
		t.Fatalf("v6 issue creation with valid label failed: %v", ierr)
	}
	if _, _, berr := repo.CreateIssue("bad label issue", "body", []string{"unknown-v6-label"}); berr == nil {
		t.Fatalf("v6 issue creation should reject unknown label")
	}
	comm, cerr := repo.CreateComment(1, "v6 benchmark verified")
	if cerr != nil || comm.ID != 1 || len(repo.ListComments(1)) != 1 {
		t.Fatalf("v6 comment creation failed: %v", cerr)
	}

	pr, perr := repo.CreatePull("Merge feature-v6", "adds feat.txt", "feature-v6", "main")
	if perr != nil || pr.Number != 1 || len(pr.Files) != 1 || pr.Files[0].Filename != "pkg/feat.txt" {
		t.Fatalf("v6 pull creation failed: %v", perr)
	}
	mergeCommit, merr := repo.MergePull(1, true)
	if merr != nil || len(mergeCommit) != 40 || !repo.GetPull(1).Merged {
		t.Fatalf("v6 pull merge failed: %v", merr)
	}
}

func TestV6GraphQLAndMcpStreamableHttpParity(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "gql-mcp-v6", "v6 parity", true)

	// Test MCP initialize, server/discover, tools/list, and tools/call
	stInit, bInit := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":10,"method":"initialize","params":{}}`), "adaiversen")
	if stInit != "200 OK" || !bytes.Contains(bInit, []byte(`"github-forge"`)) {
		t.Fatalf("v6 MCP initialize failed: %s", string(bInit))
	}
	stDisc, bDisc := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":11,"method":"server/discover","params":{}}`), "adaiversen")
	if stDisc != "200 OK" || !bytes.Contains(bDisc, []byte(`"2026-07-28"`)) {
		t.Fatalf("v6 MCP server/discover failed: %s", string(bDisc))
	}
	stList1, bList1 := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":12,"method":"tools/list","params":{}}`), "adaiversen")
	stList2, bList2 := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":12,"method":"tools/list","params":{}}`), "adaiversen")
	if stList1 != "200 OK" || stList2 != "200 OK" || !bytes.Equal(bList1, bList2) {
		t.Fatalf("v6 MCP tools/list not byte-identical")
	}

	stCall, bCall := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"create_issue","arguments":{"owner":"adaiversen","repo":"gql-mcp-v6","title":"MCP v6 Issue","body":"from mcp"}}}`), "adaiversen")
	if stCall != "200 OK" || !bytes.Contains(bCall, []byte(`MCP v6 Issue`)) {
		t.Fatalf("v6 MCP create_issue failed: %s", string(bCall))
	}

	// Verify GraphQL reads back the MCP-created issue and introspection works
	gqlRes := executeGraphQL(store, GraphQLRequest{
		Query:     `query($owner:String!, $name:String!){ repository(owner:$owner, name:$name){ nameWithOwner issue(number:1){ number title state } } }`,
		Variables: map[string]any{"owner": "adaiversen", "name": "gql-mcp-v6"},
	}, "adaiversen")
	if len(gqlRes.Errors) > 0 || gqlRes.Data == nil {
		t.Fatalf("v6 GraphQL repository query failed: %+v", gqlRes)
	}

	repoGID := toGlobalID("Repository", "1")
	gqlMut := executeGraphQL(store, GraphQLRequest{
		Query:     `mutation($input:CreateIssueInput!){ createIssue(input:$input){ issue { id number } } }`,
		Variables: map[string]any{"input": map[string]any{"repositoryId": repoGID, "title": "GQL v6 Issue", "body": "from gql"}},
	}, "adaiversen")
	if len(gqlMut.Errors) > 0 || len(repo.Issues) != 2 {
		t.Fatalf("v6 GraphQL createIssue mutation failed: %+v", gqlMut)
	}
}

func TestV6RestBranchesCommitsPullsPaginationAndErrorHandling(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	conn := newConnState()

	// 1. Test /healthz, /api/v3/, /api/v3/meta, and unauthenticated 401
	resHealth := feedAndStepAll(t, srv, conn, []byte("GET /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if len(resHealth) != 1 || !bytes.Contains(resHealth[0], []byte(`{"ok":true}`)) {
		t.Fatalf("v6 /healthz failed")
	}
	resRoot := feedAndStepAll(t, srv, conn, []byte("GET /api/v3/ HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if len(resRoot) != 1 || !bytes.Contains(resRoot[0], []byte("X-OAuth-Scopes:")) {
		t.Fatalf("v6 /api/v3/ root missing X-OAuth-Scopes")
	}
	resMeta := feedAndStepAll(t, srv, conn, []byte("GET /api/v3/meta HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if len(resMeta) != 1 || !bytes.Contains(resMeta[0], []byte("installed_version")) {
		t.Fatalf("v6 /api/v3/meta failed")
	}
	resUnauth := feedAndStepAll(t, srv, conn, []byte("GET /api/v3/user/repos HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"))
	if len(resUnauth) != 1 || !bytes.Contains(resUnauth[0], []byte("401 Unauthorized")) {
		t.Fatalf("v6 unauthenticated /api/v3/user/repos did not return 401")
	}

	// 2. Test repository pagination with Link header rel="next"
	store.CreateRepo("adaiversen", "page-repo-v6-1", "desc 1", true)
	store.CreateRepo("adaiversen", "page-repo-v6-2", "desc 2", true)
	store.CreateRepo("adaiversen", "page-repo-v6-3", "desc 3", false)

	reqPage1 := "GET /api/v3/user/repos?per_page=2&page=1 HTTP/1.1\r\nHost: 127.0.0.1:18086\r\nAuthorization: Bearer v6-tok\r\n\r\n"
	resPage1 := feedAndStepAll(t, srv, conn, []byte(reqPage1))
	if len(resPage1) != 1 || !bytes.Contains(resPage1[0], []byte(`rel="next"`)) {
		t.Fatalf("v6 pagination page 1 missing Link rel=next: %s", string(bytes.Join(resPage1, nil)))
	}
	reqPage2 := "GET /api/v3/user/repos?per_page=2&page=2 HTTP/1.1\r\nHost: 127.0.0.1:18086\r\nAuthorization: Bearer v6-tok\r\n\r\n"
	resPage2 := feedAndStepAll(t, srv, conn, []byte(reqPage2))
	if len(resPage2) != 1 || bytes.Contains(resPage2[0], []byte(`rel="next"`)) {
		t.Fatalf("v6 pagination page 2 should not have Link rel=next")
	}

	// 3. Test PATCH and DELETE repository via REST router
	patchRepoBody := `{"description":"updated-v6-desc","private":false}`
	patchRepoReq := "PATCH /api/v3/repos/adaiversen/page-repo-v6-1 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 49\r\n\r\n" + patchRepoBody
	resPatchRepo := feedAndStepAll(t, srv, conn, []byte(patchRepoReq))
	if len(resPatchRepo) != 1 || !bytes.Contains(resPatchRepo[0], []byte("updated-v6-desc")) {
		t.Fatalf("v6 PATCH repo failed: %s", string(bytes.Join(resPatchRepo, nil)))
	}

	delRepoReq := "DELETE /api/v3/repos/adaiversen/page-repo-v6-3 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\n\r\n"
	resDelRepo := feedAndStepAll(t, srv, conn, []byte(delRepoReq))
	if len(resDelRepo) != 1 || !bytes.Contains(resDelRepo[0], []byte("204 No Content")) {
		t.Fatalf("v6 DELETE repo failed")
	}

	// 4. Test REST branches, commits, contents PUT/GET/DELETE, and pull merge conflict
	repo := store.GetRepo("adaiversen", "page-repo-v6-2")
	putBody1 := `{"message":"init v6","content":"aGVsbG8gdjY="}`
	putReq1 := "PUT /api/v3/repos/adaiversen/page-repo-v6-2/contents/docs/guide.md HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 46\r\n\r\n" + putBody1
	resPut1 := feedAndStepAll(t, srv, conn, []byte(putReq1))
	if len(resPut1) != 1 || !bytes.Contains(resPut1[0], []byte("201 Created")) {
		t.Fatalf("v6 REST PUT contents failed: %s", string(bytes.Join(resPut1, nil)))
	}

	baseTip := repo.Refs["refs/heads/main"]
	createRefBody := `{"ref":"refs/heads/conflict-v6","sha":"` + baseTip + `"}`
	createRefReq := "POST /api/v3/repos/adaiversen/page-repo-v6-2/git/refs HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 81\r\n\r\n" + createRefBody
	resRef := feedAndStepAll(t, srv, conn, []byte(createRefReq))
	if len(resRef) != 1 || !bytes.Contains(resRef[0], []byte("201 Created")) {
		t.Fatalf("v6 REST POST git/refs failed: %s", string(bytes.Join(resRef, nil)))
	}

	// Modify same file on both conflict-v6 and main to trigger 405 merge conflict
	_, _, _, _ = repo.PutFile("conflict-v6", "docs/guide.md", []byte("branch change\n"), "branch edit", "adaiversen", nil)
	prConflict, _ := repo.CreatePull("Conflict PR v6", "test conflict", "conflict-v6", "main")
	_, _, _, _ = repo.PutFile("main", "docs/guide.md", []byte("main divergent change\n"), "main edit", "adaiversen", nil)

	mergeReq := "PUT /api/v3/repos/adaiversen/page-repo-v6-2/pulls/1/merge HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 25\r\n\r\n{\"merge_method\":\"squash\"}"
	resMerge := feedAndStepAll(t, srv, conn, []byte(mergeReq))
	if prConflict.Number != 1 || len(resMerge) != 1 || !bytes.Contains(resMerge[0], []byte("405 Method Not Allowed")) {
		t.Fatalf("v6 conflicting PR merge should return 405, got: %s", string(bytes.Join(resMerge, nil)))
	}

	// Test GET branches and commits
	resBranches := feedAndStepAll(t, srv, conn, []byte("GET /api/v3/repos/adaiversen/page-repo-v6-2/branches HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\n\r\n"))
	if len(resBranches) != 1 || !bytes.Contains(resBranches[0], []byte("conflict-v6")) {
		t.Fatalf("v6 GET branches failed")
	}
	resCommits := feedAndStepAll(t, srv, conn, []byte("GET /api/v3/repos/adaiversen/page-repo-v6-2/commits?sha=conflict-v6 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\n\r\n"))
	if len(resCommits) != 1 || !bytes.Contains(resCommits[0], []byte("branch edit")) {
		t.Fatalf("v6 GET commits failed")
	}
}

func TestV6GraphQLIntrospectionPaginationAndMutations(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "gql-deep-v6", "v6 deep graphql", false)
	for i := 1; i <= 6; i++ {
		_, _, _ = repo.CreateIssue("GQL Issue v6", "body", nil)
	}

	// Introspection queries
	resIssueType := executeGraphQL(store, GraphQLRequest{Query: `query { Issue: __type(name:"Issue") { fields { name } } }`}, "adaiversen")
	if resIssueType.Data == nil {
		t.Fatalf("v6 GraphQL Issue introspection failed")
	}
	resPRRepoType := executeGraphQL(store, GraphQLRequest{Query: `query { PullRequest: __type(name:"PullRequest") { fields { name } } Repository: __type(name:"Repository") { fields { name } } }`}, "adaiversen")
	if resPRRepoType.Data == nil {
		t.Fatalf("v6 GraphQL PullRequest/Repository introspection failed")
	}
	resSearchEnum := executeGraphQL(store, GraphQLRequest{Query: `query { SearchType: __type(name:"SearchType") { enumValues { name } } }`}, "adaiversen")
	if resSearchEnum.Data == nil {
		t.Fatalf("v6 GraphQL SearchType introspection failed")
	}
	resInvalid := executeGraphQL(store, GraphQLRequest{Query: `query { viewer { noSuchFieldOnViewer } }`}, "adaiversen")
	if len(resInvalid.Errors) == 0 {
		t.Fatalf("v6 GraphQL invalid field should return error")
	}

	// Close issue #2 via GraphQL closeIssue mutation and add comment via addComment
	issue2GID := toGlobalID("Issue", "adaiversen/gql-deep-v6/2")
	resClose := executeGraphQL(store, GraphQLRequest{
		Query:     `mutation($input:CloseIssueInput!){ closeIssue(input:$input){ issue { id number state } } }`,
		Variables: map[string]any{"input": map[string]any{"issueId": issue2GID}},
	}, "adaiversen")
	if len(resClose.Errors) > 0 || repo.Issues[1].State != "closed" {
		t.Fatalf("v6 GraphQL closeIssue failed")
	}

	resComment := executeGraphQL(store, GraphQLRequest{
		Query:     `mutation($input:AddCommentInput!){ addComment(input:$input){ commentEdge { node { id body } } } }`,
		Variables: map[string]any{"input": map[string]any{"subjectId": issue2GID, "body": "v6 gql comment"}},
	}, "adaiversen")
	if len(resComment.Errors) > 0 || len(repo.ListComments(2)) != 1 {
		t.Fatalf("v6 GraphQL addComment failed")
	}

	// Paginate OPEN issues in CREATED_AT DESC order
	resPage := executeGraphQL(store, GraphQLRequest{
		Query:     `query($owner:String!, $name:String!, $first:Int){ repository(owner:$owner, name:$name){ issues(first:$first, states: [OPEN], orderBy: {field: CREATED_AT, direction: DESC}){ nodes { number state } pageInfo { hasNextPage endCursor } } } }`,
		Variables: map[string]any{"owner": "adaiversen", "name": "gql-deep-v6", "first": 3},
	}, "adaiversen")
	if len(resPage.Errors) > 0 || resPage.Data == nil {
		t.Fatalf("v6 GraphQL issues pagination failed")
	}
}

func TestV6McpFullToolSuiteAndErrorContracts(t *testing.T) {
	store := NewForgeStore()

	// 1. Malformed JSON -> 400 Bad Request, -32700 Parse error
	stParseErr, bParseErr := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0", broken`), "adaiversen")
	if stParseErr != "400 Bad Request" || !bytes.Contains(bParseErr, []byte("-32700")) {
		t.Fatalf("v6 MCP parse error expected 400/-32700, got %s: %s", stParseErr, string(bParseErr))
	}

	// 2. Unknown RPC method -> 200 OK, -32601 Method not found
	stUnkMethod, bUnkMethod := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":20,"method":"no_such_method","params":{}}`), "adaiversen")
	if stUnkMethod != "200 OK" || !bytes.Contains(bUnkMethod, []byte("-32601")) {
		t.Fatalf("v6 MCP unknown method failed: %s", string(bUnkMethod))
	}

	// 3. Unknown tool in tools/call -> -32601
	stUnkTool, bUnkTool := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"unknown_v6_tool","arguments":{}}}`), "adaiversen")
	if stUnkTool != "200 OK" || !bytes.Contains(bUnkTool, []byte("-32601")) {
		t.Fatalf("v6 MCP unknown tool failed: %s", string(bUnkTool))
	}

	// 4. Missing required argument -> isError:true
	stMissing, bMissing := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"create_issue","arguments":{"owner":"adaiversen"}}}`), "adaiversen")
	if stMissing != "200 OK" || !bytes.Contains(bMissing, []byte(`"isError":true`)) {
		t.Fatalf("v6 MCP missing argument expected isError:true, got: %s", string(bMissing))
	}

	// 5. create_repo via MCP with github__ prefix
	stCreateRepo, bCreateRepo := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"github__create_repository","arguments":{"name":"mcp-v6-flow","description":"v6 mcp test","private":true}}}`), "adaiversen")
	if stCreateRepo != "200 OK" || !bytes.Contains(bCreateRepo, []byte("mcp-v6-flow")) {
		t.Fatalf("v6 MCP create_repo failed: %s", string(bCreateRepo))
	}

	// 6. get_repo via MCP
	stGetRepo, bGetRepo := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"get_repo","arguments":{"owner":"adaiversen","repo":"mcp-v6-flow"}}}`), "adaiversen")
	if stGetRepo != "200 OK" || !bytes.Contains(bGetRepo, []byte("adaiversen/mcp-v6-flow")) {
		t.Fatalf("v6 MCP get_repo failed: %s", string(bGetRepo))
	}

	// 7. Seed commits on main and feature branch, then create_pull_request and merge_pull_request via MCP
	repo := store.GetRepo("adaiversen", "mcp-v6-flow")
	mainCommit, _, _, _ := repo.PutFile("main", "init.txt", []byte("base\n"), "base commit", "adaiversen", nil)
	repo.Refs["refs/heads/mcp-head-v6"] = mainCommit
	_, _, _, _ = repo.PutFile("mcp-head-v6", "feature.txt", []byte("feature\n"), "feature commit", "adaiversen", nil)

	stCreatePR, bCreatePR := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"create_pull_request","arguments":{"owner":"adaiversen","repo":"mcp-v6-flow","title":"MCP v6 PR","head":"mcp-head-v6","base":"main"}}}`), "adaiversen")
	if stCreatePR != "200 OK" || !bytes.Contains(bCreatePR, []byte("MCP v6 PR")) {
		t.Fatalf("v6 MCP create_pull_request failed: %s", string(bCreatePR))
	}

	stMergePR, bMergePR := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"merge_pull_request","arguments":{"owner":"adaiversen","repo":"mcp-v6-flow","pull_number":1}}}`), "adaiversen")
	if stMergePR != "200 OK" || !bytes.Contains(bMergePR, []byte("Pull request successfully merged")) {
		t.Fatalf("v6 MCP merge_pull_request failed: %s", string(bMergePR))
	}

	// 8. list_issues via MCP
	_, _ = handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"create_issue","arguments":{"owner":"adaiversen","repo":"mcp-v6-flow","title":"Issue via MCP","body":"v6"}}}`), "adaiversen")
	stListIss, bListIss := handleMcpRPC(store, []byte(`{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"list_issues","arguments":{"owner":"adaiversen","repo":"mcp-v6-flow","state":"open"}}}`), "adaiversen")
	if stListIss != "200 OK" || !bytes.Contains(bListIss, []byte("Issue via MCP")) {
		t.Fatalf("v6 MCP list_issues failed: %s", string(bListIss))
	}
}

func TestV6GitShallowFetchIncrementalAndDeepenFlow(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "shallow-v6-repo", "v6 shallow test", false)

	// Build a 3-commit linear history on refs/heads/main
	c1, _, _, _ := repo.PutFile("main", "step.txt", []byte("step-1\n"), "commit 1", "adaiversen", nil)
	c2, _, _, _ := repo.PutFile("main", "step.txt", []byte("step-2\n"), "commit 2", "adaiversen", nil)
	c3, _, _, _ := repo.PutFile("main", "step.txt", []byte("step-3\n"), "commit 3", "adaiversen", nil)

	// 1. Depth-1 shallow fetch of c3
	var reqDepth1 []byte
	reqDepth1 = appendPktLineStr(reqDepth1, "command=fetch\n")
	reqDepth1 = appendPktDelim(reqDepth1)
	reqDepth1 = appendPktLineStr(reqDepth1, "want "+c3+"\n")
	reqDepth1 = appendPktLineStr(reqDepth1, "deepen 1\n")
	reqDepth1 = appendPktLineStr(reqDepth1, "done\n")
	reqDepth1 = appendPktFlush(reqDepth1)

	respDepth1 := handleGitUploadPackFastRepo(repo, reqDepth1)
	if !bytes.Contains(respDepth1, []byte("shallow-info\n")) || !bytes.Contains(respDepth1, []byte("shallow "+c3+"\n")) {
		t.Fatalf("v6 depth-1 fetch missing shallow-info for tip %s", c3)
	}

	// 2. Deepen from depth 1 to depth 2 with client shallow boundary at c3
	var reqDeepen2 []byte
	reqDeepen2 = appendPktLineStr(reqDeepen2, "command=fetch\n")
	reqDeepen2 = appendPktDelim(reqDeepen2)
	reqDeepen2 = appendPktLineStr(reqDeepen2, "want "+c3+"\n")
	reqDeepen2 = appendPktLineStr(reqDeepen2, "shallow "+c3+"\n")
	reqDeepen2 = appendPktLineStr(reqDeepen2, "deepen 2\n")
	reqDeepen2 = appendPktLineStr(reqDeepen2, "done\n")
	reqDeepen2 = appendPktFlush(reqDeepen2)

	respDeepen2 := handleGitUploadPackFastRepo(repo, reqDeepen2)
	if !bytes.Contains(respDeepen2, []byte("unshallow "+c3+"\n")) || !bytes.Contains(respDeepen2, []byte("shallow "+c2+"\n")) {
		t.Fatalf("v6 deepen-2 fetch did not move shallow boundary from %s to %s: %s", c3, c2, string(respDeepen2))
	}

	// 3. Incremental fetch with have=c2 and want=c3
	var reqIncr []byte
	reqIncr = appendPktLineStr(reqIncr, "command=fetch\n")
	reqIncr = appendPktDelim(reqIncr)
	reqIncr = appendPktLineStr(reqIncr, "want "+c3+"\n")
	reqIncr = appendPktLineStr(reqIncr, "have "+c2+"\n")
	reqIncr = appendPktLineStr(reqIncr, "done\n")
	reqIncr = appendPktFlush(reqIncr)

	respIncr := handleGitUploadPackFastRepo(repo, reqIncr)
	if !bytes.Contains(respIncr, []byte("acknowledgments\n")) || !bytes.Contains(respIncr, []byte("ready\n")) || len(c1) != 40 {
		t.Fatalf("v6 incremental fetch missing acknowledgments/ready")
	}
}


