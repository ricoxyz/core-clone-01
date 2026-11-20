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

func TestV6BenchPush200SequentialFastForwardAndNonFFMixedReport(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "bench-push-v6", "v6 sequential push benchmark simulation", false)

	prevTip := ZeroOID
	for step := 1; step <= 32; step++ {
		blobPayload := []byte("v6 sequential push benchmark payload step=" + strings.Repeat("X", step) + "\n")
		bSha := hashObject(ObjBlob, blobPayload)
		tPayload := buildTree([]TreeEntry{{Mode: "100644", Name: "bench-data.txt", OID: bSha}})
		tSha := hashObject(ObjTree, tPayload)
		var parents []string
		if prevTip != ZeroOID {
			parents = []string{prevTip}
		}
		cPayload := buildCommit(tSha, parents, "v6 bench push\n", "Bench <bench@make.fun>", int64(1700010000+step))
		cSha := hashObject(ObjCommit, cPayload)

		packBuf := writePack([]*GitObject{
			{Type: ObjBlob, Size: len(blobPayload), Data: blobPayload},
			{Type: ObjTree, Size: len(tPayload), Data: tPayload},
			{Type: ObjCommit, Size: len(cPayload), Data: cPayload},
		})

		var wireReq []byte
		wireReq = appendPktLineStr(wireReq, prevTip+" "+cSha+" refs/heads/main\x00report-status side-band-64k ofs-delta\n")
		wireReq = appendPktFlush(wireReq)
		wireReq = append(wireReq, packBuf...)

		wireResp := handleGitReceivePackRepo(repo, wireReq, nil)
		if !bytes.Contains(wireResp, []byte("unpack ok\n")) || !bytes.Contains(wireResp, []byte("ok refs/heads/main\n")) {
			t.Fatalf("v6 sequential push failed at step %d: %s", step, string(wireResp))
		}
		if !repo.hasMainTip || string(repo.mainTip[:]) != cSha {
			t.Fatalf("v6 mainTip array out of sync at step %d", step)
		}
		if !bytes.Contains(repo.recvAdvCache, []byte(cSha+" refs/heads/main\x00")) {
			t.Fatalf("v6 in-place recvAdvCache did not patch tip %s at step %d", cSha, step)
		}
		prevTip = cSha
	}

	// Verify lazy materialization recovers all 32 commits when REST or clone reads the repository
	repo.EnsureMaterialized()
	if repo.hasPendingPacks {
		t.Fatalf("v6 pendingPacks should be empty after EnsureMaterialized")
	}
	if repo.Refs["refs/heads/main"] != prevTip {
		t.Fatalf("v6 materialized main ref mismatch: got %s want %s", repo.Refs["refs/heads/main"], prevTip)
	}
	isF, fileBytes, _, _, okF := repo.GetFileOrDir("main", "bench-data.txt")
	if !okF || !isF || !bytes.HasPrefix(fileBytes, []byte("v6 sequential push benchmark payload")) {
		t.Fatalf("v6 materialized file content mismatch")
	}

	// Test mixed multi-ref push: one valid branch creation + one non-fast-forward update on refs/heads/main
	divBlob := []byte("divergent v6 root blob\n")
	divBlobSha := hashObject(ObjBlob, divBlob)
	divTree := buildTree([]TreeEntry{{Mode: "100644", Name: "div.txt", OID: divBlobSha}})
	divTreeSha := hashObject(ObjTree, divTree)
	divCommit := buildCommit(divTreeSha, nil, "divergent root\n", "Bench <bench@make.fun>", 1700020000)
	divCommitSha := hashObject(ObjCommit, divCommit)

	divPack := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(divBlob), Data: divBlob},
		{Type: ObjTree, Size: len(divTree), Data: divTree},
		{Type: ObjCommit, Size: len(divCommit), Data: divCommit},
	})

	var mixedReq []byte
	mixedReq = appendPktLineStr(mixedReq, ZeroOID+" "+divCommitSha+" refs/heads/good-branch\x00report-status side-band-64k ofs-delta\n")
	mixedReq = appendPktLineStr(mixedReq, prevTip+" "+divCommitSha+" refs/heads/main\n")
	mixedReq = appendPktFlush(mixedReq)
	mixedReq = append(mixedReq, divPack...)

	mixedResp := handleGitReceivePackRepo(repo, mixedReq, nil)
	if !bytes.Contains(mixedResp, []byte("ok refs/heads/good-branch\n")) {
		t.Fatalf("v6 mixed push should accept good-branch: %s", string(mixedResp))
	}
	if !bytes.Contains(mixedResp, []byte("ng refs/heads/main non-fast-forward\n")) {
		t.Fatalf("v6 mixed push should reject non-fast-forward main: %s", string(mixedResp))
	}
	if repo.Refs["refs/heads/main"] != prevTip {
		t.Fatalf("v6 rejected non-fast-forward moved main ref")
	}
}

func TestV6PerFDHotSocketReadAndWriteBenchmarkSimulation(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	store.CreateRepo("adaiversen", "bench-rw-v6", "v6 read/write bench", true)

	conn := newConnState()
	// 1. Run 50 sequential POST /issues on the same connection to exercise lastPostRepo cache & compact 201 path
	for i := 1; i <= 50; i++ {
		bodyStr := `{"title":"Issue title [bench ` + string(rune('0'+(i%10))) + `]","body":"v6 benchmark issue body"}`
		hdrStr := "POST /api/v3/repos/adaiversen/bench-rw-v6/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-secret\r\nContent-Length: 66\r\n\r\n" + bodyStr
		out := feedAndStepAll(t, srv, conn, []byte(hdrStr))
		if len(out) != 1 || !bytes.Contains(out[0], []byte("HTTP/1.1 201 Created")) || !bytes.Contains(out[0], []byte(`{"number":`)) {
			t.Fatalf("v6 sequential POST issue %d failed: %s", i, string(bytes.Join(out, nil)))
		}
	}
	if conn.lastPostRepo == nil || conn.lastPostLen == 0 {
		t.Fatalf("v6 per-FD lastPostRepo cache was not populated")
	}

	// 2. Create a bench setup issue and run 100 sequential GET /issues/51 on the same connection
	setupBody := `{"title":"Setup issue (bench setup)","body":"v6 read setup"}`
	setupReq := "POST /api/v3/repos/adaiversen/bench-rw-v6/issues HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-secret\r\nContent-Length: 60\r\n\r\n" + setupBody
	outSetup := feedAndStepAll(t, srv, conn, []byte(setupReq))
	if len(outSetup) != 1 || !bytes.Contains(outSetup[0], []byte("201 Created")) {
		t.Fatalf("v6 setup issue create failed")
	}

	getIssue51 := []byte("GET /api/v3/repos/adaiversen/bench-rw-v6/issues/51 HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-secret\r\n\r\n")
	for i := 1; i <= 100; i++ {
		outGet := feedAndStepAll(t, srv, conn, getIssue51)
		if len(outGet) != 1 || !bytes.Contains(outGet[0], []byte(`{"number":51}`)) {
			t.Fatalf("v6 sequential GET issue 51 iteration %d failed: %s", i, string(bytes.Join(outGet, nil)))
		}
	}

	// 3. Delete repository and verify per-FD GET & POST caches invalidate via store.mutationGen
	store.DeleteRepo("adaiversen", "bench-rw-v6")
	outAfterDel := feedAndStepAll(t, srv, conn, getIssue51)
	if len(outAfterDel) != 1 || !bytes.Contains(outAfterDel[0], []byte("404 Not Found")) {
		t.Fatalf("v6 per-FD GET cache did not invalidate on DeleteRepo: %s", string(bytes.Join(outAfterDel, nil)))
	}
}

func TestV6JsonEscapingAndSimpleIssueParserEdgeCases(t *testing.T) {
	// Verify parseSimpleIssueJSON handles escaped quotes, newlines, tabs, backslashes, and rejects labels key
	cases := []struct {
		raw       string
		wantOK    bool
		wantTitle string
		wantBody  string
	}{
		{
			raw:       `{"title":"simple v6 title","body":"simple v6 body"}`,
			wantOK:    true,
			wantTitle: "simple v6 title",
			wantBody:  "simple v6 body",
		},
		{
			raw:       `{ "body" : "line1\nline2\t\"quoted\"\\slash\/end" , "title" : "escaped\r\ntitle" }`,
			wantOK:    true,
			wantTitle: "escaped\r\ntitle",
			wantBody:  "line1\nline2\t\"quoted\"\\slash/end",
		},
		{
			raw:    `{"title":"with labels","body":"fallback","labels":["bug"]}`,
			wantOK: false,
		},
		{
			raw:    `{"title":"unicode \u0041","body":"fallback"}`,
			wantOK: false,
		},
		{
			raw:    `not-a-json-object`,
			wantOK: false,
		},
	}

	for idx, tc := range cases {
		gotTitle, gotBody, ok := parseSimpleIssueJSON([]byte(tc.raw))
		if ok != tc.wantOK {
			t.Fatalf("v6 parseSimpleIssueJSON case %d ok=%v want %v", idx, ok, tc.wantOK)
		}
		if ok && (gotTitle != tc.wantTitle || gotBody != tc.wantBody) {
			t.Fatalf("v6 parseSimpleIssueJSON case %d got (%q, %q), want (%q, %q)", idx, gotTitle, gotBody, tc.wantTitle, tc.wantBody)
		}
	}

	// Verify appendJSONString escapes control characters and quotes properly
	escaped := string(appendJSONString(nil, "a\"b\\c\nd\re\tf\x01g"))
	if escaped != `"a\"b\\c\nd\re\tf\u0001g"` {
		t.Fatalf("v6 appendJSONString unexpected output: %s", escaped)
	}
}

func TestV6GraphQLBatchNodeAndPullRequestLifecycle(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	conn := newConnState()

	repo := store.CreateRepo("adaiversen", "gql-pr-v6", "v6 graphql pr lifecycle", false)
	baseSha, _, _, _ := repo.PutFile("main", "README.md", []byte("# v6\n"), "init readme", "adaiversen", nil)
	repo.Refs["refs/heads/gql-branch-v6"] = baseSha
	_, _, _, _ = repo.PutFile("gql-branch-v6", "v6.txt", []byte("v6 feature\n"), "add v6.txt", "adaiversen", nil)

	repoGID := toGlobalID("Repository", "1")

	// 1. Node query by Global ID
	nodeRes := executeGraphQL(store, GraphQLRequest{
		Query:     `query($id:ID!){ node(id:$id){ ... on Repository { __typename id name nameWithOwner } } }`,
		Variables: map[string]any{"id": repoGID},
	}, "adaiversen")
	if len(nodeRes.Errors) > 0 || nodeRes.Data == nil {
		t.Fatalf("v6 GraphQL node query failed: %+v", nodeRes)
	}

	// 2. createPullRequest mutation
	createPRRes := executeGraphQL(store, GraphQLRequest{
		Query: `mutation($input:CreatePullRequestInput!){ createPullRequest(input:$input){ pullRequest { id number state headRefName baseRefName } } }`,
		Variables: map[string]any{
			"input": map[string]any{
				"repositoryId": repoGID,
				"title":        "GQL v6 Pull Request",
				"body":         "created over GraphQL v4",
				"headRefName":  "gql-branch-v6",
				"baseRefName":  "main",
			},
		},
	}, "adaiversen")
	if len(createPRRes.Errors) > 0 || len(repo.Pulls) != 1 {
		t.Fatalf("v6 GraphQL createPullRequest failed: %+v", createPRRes)
	}

	// 3. Query pullRequest(number:1) with files and pullRequests list
	prQueryRes := executeGraphQL(store, GraphQLRequest{
		Query: `query { repository(owner:"adaiversen", name:"gql-pr-v6") { pullRequests(first:10) { nodes { number state } } pullRequest(number:1) { number state mergeable files(first:10) { nodes { path } } } } }`,
	}, "adaiversen")
	if len(prQueryRes.Errors) > 0 || prQueryRes.Data == nil {
		t.Fatalf("v6 GraphQL pullRequest query failed: %+v", prQueryRes)
	}

	// 4. mergePullRequest mutation
	prGID := toGlobalID("PullRequest", "adaiversen/gql-pr-v6/1")
	mergePRRes := executeGraphQL(store, GraphQLRequest{
		Query:     `mutation($input:MergePullRequestInput!){ mergePullRequest(input:$input){ pullRequest { number merged state } } }`,
		Variables: map[string]any{"input": map[string]any{"pullRequestId": prGID}},
	}, "adaiversen")
	if len(mergePRRes.Errors) > 0 || !repo.Pulls[0].Merged {
		t.Fatalf("v6 GraphQL mergePullRequest failed: %+v", mergePRRes)
	}

	// 5. Batched GraphQL HTTP POST via Server.stepConn
	batchJSON := `[{"query":"query { viewer { login } }"},{"query":"query { repository(owner:\"adaiversen\", name:\"missing-v6\") { id } }"}]`
	batchReq := "POST /api/graphql HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nContent-Length: 123\r\n\r\n" + batchJSON
	batchOut := feedAndStepAll(t, srv, conn, []byte(batchReq))
	if len(batchOut) != 1 || !bytes.Contains(batchOut[0], []byte(`"login":"adaiversen"`)) || !bytes.Contains(batchOut[0], []byte(`"repository":null`)) {
		t.Fatalf("v6 GraphQL batched HTTP POST failed: %s", string(bytes.Join(batchOut, nil)))
	}
}

func TestV6GitProtocolV0UploadPackAndRefDeltaResolution(t *testing.T) {
	store := NewForgeStore()
	repo := store.CreateRepo("adaiversen", "v0-refdelta-v6", "v6 v0 upload-pack & ref-delta", false)

	// 1. Seed base blob and tree
	b0 := []byte("v6-base-payload-for-ref-delta-verification-line-01\n")
	b0Sha := hashObject(ObjBlob, b0)
	t0 := buildTree([]TreeEntry{{Mode: "100644", Name: "delta.txt", OID: b0Sha}})
	t0Sha := hashObject(ObjTree, t0)
	c0 := buildCommit(t0Sha, nil, "v6 v0 root commit\n", "V6 Tester <v6@make.fun>", 1700050000)
	c0Sha := hashObject(ObjCommit, c0)

	initPack := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(b0), Data: b0},
		{Type: ObjTree, Size: len(t0), Data: t0},
		{Type: ObjCommit, Size: len(c0), Data: c0},
	})
	if rerr := readPackIntoRepo(initPack, repo); rerr != nil {
		t.Fatalf("v6 initial pack unpack failed: %v", rerr)
	}
	repo.Refs["refs/heads/main"] = c0Sha
	copy(repo.mainTip[:], c0Sha)
	repo.hasMainTip = true

	// 2. Test Protocol v0 fetch (no command=fetch header, two flush sections: wants +done)
	var v0FetchReq []byte
	v0FetchReq = appendPktLineStr(v0FetchReq, "want "+c0Sha+" multi_ack side-band-64k ofs-delta shallow\n")
	v0FetchReq = appendPktLineStr(v0FetchReq, "deepen 1\n")
	v0FetchReq = appendPktFlush(v0FetchReq)
	v0FetchReq = appendPktLineStr(v0FetchReq, "done\n")
	v0FetchReq = appendPktFlush(v0FetchReq)

	v0FetchResp := handleGitUploadPackRepo(repo, v0FetchReq)
	if !bytes.Contains(v0FetchResp, []byte("NAK\n")) || !bytes.Contains(v0FetchResp, []byte("PACK")) {
		t.Fatalf("v6 protocol v0 fetch response missing NAK or PACK")
	}

	// 3. Test non-sideband receive-pack report-status framing
	b1 := []byte("v6-updated-payload-without-sideband-capability\n")
	b1Sha := hashObject(ObjBlob, b1)
	t1 := buildTree([]TreeEntry{{Mode: "100644", Name: "delta.txt", OID: b1Sha}})
	t1Sha := hashObject(ObjTree, t1)
	c1 := buildCommit(t1Sha, []string{c0Sha}, "v6 non-sideband commit\n", "V6 Tester <v6@make.fun>", 1700050010)
	c1Sha := hashObject(ObjCommit, c1)

	pack1 := writePack([]*GitObject{
		{Type: ObjBlob, Size: len(b1), Data: b1},
		{Type: ObjTree, Size: len(t1), Data: t1},
		{Type: ObjCommit, Size: len(c1), Data: c1},
	})
	var nonSbPush []byte
	nonSbPush = appendPktLineStr(nonSbPush, c0Sha+" "+c1Sha+" refs/heads/main\x00report-status ofs-delta\n")
	nonSbPush = appendPktFlush(nonSbPush)
	nonSbPush = append(nonSbPush, pack1...)

	nonSbResp := handleGitReceivePackRepo(repo, nonSbPush, nil)
	if !bytes.Contains(nonSbResp, []byte("000eunpack ok\n0018ok refs/heads/main\n0000")) {
		t.Fatalf("v6 non-sideband push response mismatch: %s", string(nonSbResp))
	}
}

func TestV6RestNestedDirectoriesDeleteAndIssueSorting(t *testing.T) {
	store := NewForgeStore()
	srv := NewServer(store)
	conn := newConnState()

	repo := store.CreateRepo("adaiversen", "nested-dir-v6", "v6 nested tree & issue sort", true)

	// 1. Create deeply nested files across 3 directory levels
	_, _, _, err1 := repo.PutFile("main", "a/b/c/deep1.txt", []byte("deep-v6-1"), "commit deep1", "adaiversen", nil)
	_, sha2, _, err2 := repo.PutFile("main", "a/b/c/deep2.txt", []byte("deep-v6-2"), "commit deep2", "adaiversen", nil)
	_, _, _, err3 := repo.PutFile("main", "a/b/sibling.txt", []byte("sibling-v6"), "commit sibling", "adaiversen", nil)
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("v6 nested PutFile failed: %v %v %v", err1, err2, err3)
	}

	// 2. Verify directory listing at root "", "a", "a/b", and "a/b/c"
	isF0, _, _, entriesRoot, ok0 := repo.GetFileOrDir("main", "")
	if !ok0 || isF0 || len(entriesRoot) != 1 || entriesRoot[0].Name != "a" || entriesRoot[0].Type != "dir" {
		t.Fatalf("v6 root directory listing unexpected: %+v", entriesRoot)
	}
	isF1, _, _, entriesAB, ok1 := repo.GetFileOrDir("main", "a/b")
	if !ok1 || isF1 || len(entriesAB) != 2 {
		t.Fatalf("v6 a/b directory listing expected 2 entries (c and sibling.txt), got: %+v", entriesAB)
	}
	isF2, _, _, entriesABC, ok2 := repo.GetFileOrDir("main", "a/b/c")
	if !ok2 || isF2 || len(entriesABC) != 2 {
		t.Fatalf("v6 a/b/c directory listing expected 2 files, got: %+v", entriesABC)
	}

	// 3. Delete deep2.txt with stale SHA (expect error) then valid SHA (expect success)
	stale := "ffffffffffffffffffffffffffffffffffffffff"
	if _, derr := repo.DeleteFile("main", "a/b/c/deep2.txt", "del stale", "adaiversen", &stale); derr == nil {
		t.Fatalf("v6 DeleteFile should fail on stale SHA")
	}
	delCommit, derr := repo.DeleteFile("main", "a/b/c/deep2.txt", "del valid", "adaiversen", &sha2)
	if derr != nil || len(delCommit) != 40 {
		t.Fatalf("v6 DeleteFile with valid SHA failed: %v", derr)
	}
	_, _, _, entriesAfterDel, okAfter := repo.GetFileOrDir("main", "a/b/c")
	if !okAfter || len(entriesAfterDel) != 1 || entriesAfterDel[0].Name != "deep1.txt" {
		t.Fatalf("v6 directory listing after DeleteFile unexpected: %+v", entriesAfterDel)
	}

	// 4. Test REST DELETE /contents/{path} and GET /issues sorting (asc vs desc) & state filtering
	_, _, _ = repo.CreateIssue("Issue Alpha v6", "alpha", nil)
	_, _, _ = repo.CreateIssue("Issue Beta v6", "beta", nil)
	_, _, _ = repo.CreateIssue("Issue Gamma v6", "gamma", nil)
	closedState := "closed"
	_, _ = repo.PatchIssue(2, &closedState, nil, nil)

	openAsc := repo.ListIssues("open", "asc")
	if len(openAsc) != 2 || openAsc[0].Number != 1 || openAsc[1].Number != 3 {
		t.Fatalf("v6 ListIssues(open, asc) expected [1, 3], got len=%d", len(openAsc))
	}
	openDesc := repo.ListIssues("open", "desc")
	if len(openDesc) != 2 || openDesc[0].Number != 3 || openDesc[1].Number != 1 {
		t.Fatalf("v6 ListIssues(open, desc) expected [3, 1], got len=%d", len(openDesc))
	}
	allDesc := repo.ListIssues("all", "desc")
	if len(allDesc) != 3 || allDesc[0].Number != 3 || allDesc[1].Number != 2 || allDesc[2].Number != 1 {
		t.Fatalf("v6 ListIssues(all, desc) expected [3, 2, 1]")
	}

	// 5. Verify HTTP Connection: close flag and unknown route 404 JSON format
	closeReq := "GET /api/v3/unknown-v6-endpoint HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer v6-tok\r\nConnection: close\r\n\r\n"
	conn.prepareReadWindow()
	copy(conn.buf[conn.end:], closeReq)
	conn.end += len(closeReq)
	resp, needMore, _, closeAfter, fatal := srv.stepConn(conn, nil)
	if fatal || needMore || !closeAfter || !bytes.Contains(resp, []byte("404 Not Found")) {
		t.Fatalf("v6 Connection: close or 404 handling failed: closeAfter=%v resp=%s", closeAfter, string(resp))
	}
}

func TestV6EnvUrlParsingAndRetryBackoffSchedule(t *testing.T) {
	hp1, hh1, bp1 := parseEnvBase("http://store:8080/env/v1/")
	if hp1 != "store:8080" || hh1 != "store:8080" || bp1 != "/env/v1" {
		t.Fatalf("v6 parseEnvBase with port & path failed: %s %s %s", hp1, hh1, bp1)
	}
	hp2, hh2, bp2 := parseEnvBase("http://env-host")
	if hp2 != "env-host:80" || hh2 != "env-host" || bp2 != "" {
		t.Fatalf("v6 parseEnvBase default port 80 failed: %s %s %s", hp2, hh2, bp2)
	}

	if d0 := retryDelay(0); d0.Milliseconds() != 250 {
		t.Fatalf("v6 retryDelay(0) expected 250ms, got %v", d0)
	}
	if d9 := retryDelay(9); d9.Milliseconds() != 2000 {
		t.Fatalf("v6 retryDelay(9) expected cap at 2000ms, got %v", d9)
	}
}




