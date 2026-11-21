package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

var (
	uploadPackV2AdvHTTP    []byte
	respPushMainOKSideband []byte
	mainRefBytes           = []byte("refs/heads/main")
)

func init() {
	var body []byte
	body = appendPktLineStr(body, "version 2\n")
	body = appendPktLineStr(body, "ls-refs=peel symrefs\n")
	body = appendPktLineStr(body, "fetch=shallow\n")
	body = appendPktFlush(body)
	uploadPackV2AdvHTTP = buildHTTPGitFrame(
		"application/x-git-upload-pack-advertisement",
		body,
	)

	var rb []byte
	rb = appendPktLineStr(rb, "unpack ok\n")
	rb = appendPktLineStr(rb, "ok refs/heads/main\n")
	rb = appendPktFlush(rb)
	var sb []byte
	sz := len(rb) + 5
	const hexDigits = "0123456789abcdef"
	sb = append(sb,
		hexDigits[(sz>>12)&0xf],
		hexDigits[(sz>>8)&0xf],
		hexDigits[(sz>>4)&0xf],
		hexDigits[sz&0xf],
		1,
	)
	sb = append(sb, rb...)
	sb = appendPktFlush(sb)
	respPushMainOKSideband = buildHTTPGitFrame("application/x-git-receive-pack-result", sb)
}

func handleGitInfoRefsBytes(store *ForgeStore, owner, repoName, query, gitProto []byte) []byte {
	repo := store.GetRepoBytes(owner, repoName)
	if repo == nil {
		return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
	}

	if bytes.Equal(query, []byte("service=git-upload-pack")) {
		if bytes.Contains(gitProto, []byte("version=2")) {
			return uploadPackV2AdvHTTP
		}
	} else if bytes.Equal(query, []byte("service=git-receive-pack")) {
		if len(repo.recvAdvCache) > 0 {
			return repo.recvAdvCache
		}
	}
	return handleGitInfoRefs(store, string(owner), string(repoName), string(query), string(gitProto))
}

func handleGitInfoRefs(store *ForgeStore, owner, repoName, query, gitProto string) []byte {
	repo := store.GetRepo(owner, strings.TrimSuffix(repoName, ".git"))
	if repo == nil {
		return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
	}

	service := extractQueryParam(query, "service")
	if service == "git-upload-pack" {
		if strings.Contains(gitProto, "version=2") {
			return uploadPackV2AdvHTTP
		}
		// v0 advertisement
		repo.syncMainRefString()
		var body []byte
		body = appendPktLineStr(body, "# service=git-upload-pack\n")
		body = appendPktFlush(body)
		const caps = "multi_ack side-band-64k ofs-delta shallow agent=makefun-git/2.0"

		refs := sortedRefsLocked(repo)
		if len(refs) == 0 {
			body = appendPktLineStr(body, ZeroOID+" capabilities^{}\x00"+caps+"\n")
		} else {
			headTip := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if headTip == "" {
				headTip = refs[0].oid
			}
			body = appendPktLineStr(body, headTip+" HEAD\x00symref=HEAD:refs/heads/"+repo.DefaultBranch+" "+caps+"\n")
			for _, rf := range refs {
				body = appendPktLineStr(body, rf.oid+" "+rf.name+"\n")
			}
		}
		body = appendPktFlush(body)
		return buildHTTPFrame("200 OK", "application/x-git-upload-pack-advertisement", body, "Cache-Control: no-cache\r\n")
	}

	if service == "git-receive-pack" {
		if len(repo.recvAdvCache) > 0 {
			return repo.recvAdvCache
		}
		resp := buildRecvAdvHTTPLocked(repo)
		repo.recvAdvCache = resp
		return resp
	}

	return buildHTTPJSONFrame("400 Bad Request", []byte(`{"message":"Unknown service"}`), "")
}

type refPair struct {
	name string
	oid  string
}

func sortedRefsLocked(repo *StoredRepo) []refPair {
	repo.syncMainRefString()
	refs := make([]refPair, 0, len(repo.Refs))
	for k, v := range repo.Refs {
		if strings.HasPrefix(k, "refs/") {
			refs = append(refs, refPair{name: k, oid: v})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func patchRecvAdvBuf(repo *StoredRepo, oid []byte) []byte {
	if isSingleReactor {
		copy(repo.recvAdvBuf[repo.recvAdvOIDOff:repo.recvAdvOIDOff+40], oid)
		return repo.recvAdvBuf
	}
	frame := repo.allocArenaLocked(len(repo.recvAdvBuf))
	copy(frame, repo.recvAdvBuf)
	copy(frame[repo.recvAdvOIDOff:repo.recvAdvOIDOff+40], oid)
	return frame
}

func buildRecvAdvMainInPlace(repo *StoredRepo) []byte {
	const caps = "report-status delete-refs side-band-64k ofs-delta"
	if repo.recvAdvRef == "refs/heads/main" && repo.recvAdvOIDOff > 0 && len(repo.recvAdvBuf) > 0 {
		return patchRecvAdvBuf(repo, repo.mainTip[:])
	}
	body := make([]byte, 0, 160)
	body = appendPktLineStr(body, "# service=git-receive-pack\n")
	body = appendPktFlush(body)
	oidBodyOff := len(body) + 4
	body = appendPktLineStr(body, string(repo.mainTip[:])+" refs/heads/main\x00"+caps+"\n")
	body = appendPktFlush(body)
	frame := buildHTTPGitFrame("application/x-git-receive-pack-advertisement", body)
	hdrLen := len(frame) - len(body)
	repo.recvAdvBuf = frame
	repo.recvAdvRef = "refs/heads/main"
	repo.recvAdvOIDOff = hdrLen + oidBodyOff
	if isSingleReactor {
		return frame
	}
	out := repo.allocArenaLocked(len(frame))
	copy(out, frame)
	return out
}

func buildRecvAdvHTTPLocked(repo *StoredRepo) []byte {
	const caps = "report-status delete-refs side-band-64k ofs-delta"
	if repo.hasMainTip && len(repo.Refs) <= 1 {
		if len(repo.Refs) == 0 {
			return buildRecvAdvMainInPlace(repo)
		}
		if _, ok := repo.Refs["refs/heads/main"]; ok {
			return buildRecvAdvMainInPlace(repo)
		}
	}
	repo.syncMainRefString()
	if len(repo.Refs) == 1 {
		for k, v := range repo.Refs {
			if strings.HasPrefix(k, "refs/") && len(v) == 40 {
				if repo.recvAdvRef == k && repo.recvAdvOIDOff > 0 && len(repo.recvAdvBuf) > 0 {
					return patchRecvAdvBuf(repo, []byte(v))
				}
				body := make([]byte, 0, 160)
				body = appendPktLineStr(body, "# service=git-receive-pack\n")
				body = appendPktFlush(body)
				oidBodyOff := len(body) + 4
				body = appendPktLineStr(body, v+" "+k+"\x00"+caps+"\n")
				body = appendPktFlush(body)
				frame := buildHTTPGitFrame("application/x-git-receive-pack-advertisement", body)
				hdrLen := len(frame) - len(body)
				repo.recvAdvBuf = frame
				repo.recvAdvRef = k
				repo.recvAdvOIDOff = hdrLen + oidBodyOff
				if isSingleReactor {
					return frame
				}
				out := repo.allocArenaLocked(len(frame))
				copy(out, frame)
				return out
			}
		}
	}
	repo.recvAdvRef = ""
	body := make([]byte, 0, 224)
	body = appendPktLineStr(body, "# service=git-receive-pack\n")
	body = appendPktFlush(body)
	if len(repo.Refs) == 0 {
		body = appendPktLineStr(body, ZeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		refs := sortedRefsLocked(repo)
		if len(refs) == 0 {
			body = appendPktLineStr(body, ZeroOID+" capabilities^{}\x00"+caps+"\n")
		} else {
			for i, rf := range refs {
				if i == 0 {
					body = appendPktLineStr(body, rf.oid+" "+rf.name+"\x00"+caps+"\n")
				} else {
					body = appendPktLineStr(body, rf.oid+" "+rf.name+"\n")
				}
			}
		}
	}
	body = appendPktFlush(body)
	return buildHTTPGitFrame("application/x-git-receive-pack-advertisement", body)
}

func handleGitUploadPackBytes(store *ForgeStore, owner, repoName, rawBody []byte) []byte {
	repo := store.GetRepoBytes(owner, repoName)
	if repo == nil {
		return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
	}
	return handleGitUploadPackFastRepo(repo, rawBody)
}

var (
	pktCmdLsRefs   = []byte("0014command=ls-refs\n")
	pktCmdFetch    = []byte("0012command=fetch\n")
	pktDeepen1Tail = []byte("000ddeepen 1\n0009done\n0000")
	pktWantPrefix  = []byte("0032want ")
)

func handleGitUploadPackFastRepo(repo *StoredRepo, rawBody []byte) []byte {
	// Zero-scan fixed-offset / single-pass pkt-line fast path for Protocol v2 ls-refs and depth-1 clone fetch
	if len(rawBody) >= 24 && bytes.HasPrefix(rawBody, pktCmdLsRefs) {
		if len(repo.lsRefsAllCache) > 0 {
			pos := 20
			hasPeel := false
			hasSymrefs := false
			ok := true
			seenFlush := false
			for pos+4 <= len(rawBody) {
				n, err := parseHex4(rawBody[pos : pos+4])
				if err != nil {
					ok = false
					break
				}
				pos += 4
				if n == 0 {
					seenFlush = pos == len(rawBody)
					break
				}
				if n == 1 {
					continue
				}
				if n < 4 || pos+n-4 > len(rawBody) {
					ok = false
					break
				}
				pl := rawBody[pos : pos+n-4]
				pos += n - 4
				if bytes.Equal(pl, []byte("peel\n")) {
					hasPeel = true
				} else if bytes.Equal(pl, []byte("symrefs\n")) {
					hasSymrefs = true
				} else if bytes.HasPrefix(pl, []byte("ref-prefix ")) {
					ok = false
					break
				}
			}
			if ok && seenFlush && hasPeel && hasSymrefs {
				return repo.lsRefsAllCache
			}
		}
	} else if len(rawBody) >= 90 && bytes.HasPrefix(rawBody, pktCmdFetch) {
		if cached := repo.cloneDepth1Cache; cached != nil && bytes.HasSuffix(rawBody, pktDeepen1Tail) {
			// Standard depth-1 fetch ends with: 0032want <40-hex>\n000ddeepen 1\n0009done\n0000
			wantStart := len(rawBody) - len(pktDeepen1Tail) - 50
			if wantStart >= 18 &&
				bytes.Equal(rawBody[wantStart:wantStart+9], pktWantPrefix) &&
				rawBody[wantStart+49] == '\n' &&
				bytes.Equal(rawBody[wantStart+9:wantStart+49], cached.tipOID[:]) {
				pos := 18
				ok := true
				for pos < wantStart {
					if pos+4 > wantStart {
						ok = false
						break
					}
					n, err := parseHex4(rawBody[pos : pos+4])
					if err != nil {
						ok = false
						break
					}
					pos += 4
					if n == 1 {
						continue
					}
					if n < 4 || pos+n-4 > wantStart {
						ok = false
						break
					}
					pl := rawBody[pos : pos+n-4]
					pos += n - 4
					if bytes.HasPrefix(pl, []byte("want ")) || bytes.HasPrefix(pl, []byte("have ")) || bytes.HasPrefix(pl, []byte("shallow ")) {
						ok = false
						break
					}
				}
				if ok && pos == wantStart {
					return cached.httpResp
				}
			}
		}
	}

	return handleGitUploadPackRepo(repo, rawBody)
}

func handleGitUploadPack(store *ForgeStore, owner, repoName string, rawBody []byte) []byte {
	repo := store.GetRepo(owner, strings.TrimSuffix(repoName, ".git"))
	if repo == nil {
		return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
	}
	return handleGitUploadPackRepo(repo, rawBody)
}

func handleGitUploadPackRepo(repo *StoredRepo, rawBody []byte) []byte {
	pkts, _, err := decodeAllPkts(rawBody)
	if err != nil {
		return buildHTTPJSONFrame("400 Bad Request", []byte(`{"message":"Malformed pkt-line"}`), "")
	}

	command := ""
	var args []string
	for _, p := range pkts {
		if p.Kind != PktData {
			continue
		}
		line := string(bytes.TrimSuffix(p.Payload, []byte{'\n'}))
		if strings.HasPrefix(line, "command=") {
			command = line[8:]
		} else {
			args = append(args, line)
		}
	}

	isV0 := false
	if command == "" {
		for _, a := range args {
			if strings.HasPrefix(a, "want ") {
				command = "fetch"
				isV0 = true
				break
			}
		}
	} else if command == "fetch" && !bytes.Contains(rawBody, []byte("command=fetch")) {
		isV0 = true
	}

	if command == "ls-refs" {
		peel := false
		symrefs := false
		var prefixes []string
		for _, a := range args {
			switch {
			case a == "peel":
				peel = true
			case a == "symrefs":
				symrefs = true
			case strings.HasPrefix(a, "ref-prefix "):
				prefixes = append(prefixes, a[11:])
			}
		}

		if peel && symrefs && len(prefixes) == 0 {
			if len(repo.lsRefsAllCache) > 0 {
				return repo.lsRefsAllCache
			}
		} else if peel && symrefs && len(prefixes) == 2 &&
			((prefixes[0] == "HEAD" && prefixes[1] == "refs/heads/") || (prefixes[1] == "HEAD" && prefixes[0] == "refs/heads/")) {
			if len(repo.lsRefsHeadsCache) > 0 {
				return repo.lsRefsHeadsCache
			}
		}

		matchesPrefix := func(name string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, pf := range prefixes {
				if strings.HasPrefix(name, pf) {
					return true
				}
			}
			return false
		}

		repo.EnsureMaterialized()
		refs := sortedRefsLocked(repo)
		var body []byte
		if len(refs) == 0 {
			if symrefs && matchesPrefix("HEAD") {
				body = appendPktLineStr(body, "unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n")
			}
			body = appendPktFlush(body)
			return buildHTTPFrame("200 OK", "application/x-git-upload-pack-result", body, "Cache-Control: no-cache\r\n")
		}

		if matchesPrefix("HEAD") {
			if headTip := repo.Refs["refs/heads/"+repo.DefaultBranch]; headTip != "" {
				line := headTip + " HEAD"
				if symrefs {
					line += " symref-target:refs/heads/" + repo.DefaultBranch
				}
				body = appendPktLineStr(body, line+"\n")
			}
		}
		for _, rf := range refs {
			if !matchesPrefix(rf.name) {
				continue
			}
			line := rf.oid + " " + rf.name
			if peel && strings.HasPrefix(rf.name, "refs/tags/") {
				if tagObj, ok := repo.Objects[rf.oid]; ok && tagObj.Type == ObjTag {
					pt := parseTag(tagObj.Data)
					if pt.Object != "" {
						line += " peeled:" + pt.Object
					}
				}
			}
			body = appendPktLineStr(body, line+"\n")
		}
		body = appendPktFlush(body)
		resp := buildHTTPGitFrame("application/x-git-upload-pack-result", body)
		if peel && symrefs && len(prefixes) == 0 {
			repo.lsRefsAllCache = resp
		} else if peel && symrefs && len(prefixes) == 2 &&
			((prefixes[0] == "HEAD" && prefixes[1] == "refs/heads/") || (prefixes[1] == "HEAD" && prefixes[0] == "refs/heads/")) {
			repo.lsRefsHeadsCache = resp
		}
		return resp
	}

	if command == "fetch" {
		var wants []string
		var haves []string
		var clientShallows []string
		depth := 0
		hasDepth := false

		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "want "):
				tok := firstWord(a[5:])
				if isObjectID(tok) {
					wants = append(wants, tok)
				}
			case strings.HasPrefix(a, "have "):
				tok := firstWord(a[5:])
				if isObjectID(tok) {
					haves = append(haves, tok)
				}
			case strings.HasPrefix(a, "shallow "):
				tok := firstWord(a[8:])
				if isObjectID(tok) {
					clientShallows = append(clientShallows, tok)
				}
			case strings.HasPrefix(a, "deepen "):
				if d, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
					depth = d
					hasDepth = true
				}
			}
		}

		isDepth1Cacheable := !isV0 && hasDepth && depth == 1 && len(wants) == 1 && len(haves) == 0 && len(clientShallows) == 0
		if isDepth1Cacheable {
			if cached := repo.cloneDepth1Cache; cached != nil && string(cached.tipOID[:]) == wants[0] {
				return cached.httpResp
			}
		}

		repo.EnsureMaterialized()

		stopAt := make(map[string]struct{}, len(clientShallows))
		for _, s := range clientShallows {
			stopAt[s] = struct{}{}
		}
		havesReachable := make(map[string]struct{}, len(haves)*4)
		if len(haves) > 0 {
			q := make([]string, len(haves))
			copy(q, haves)
			for len(q) > 0 {
				oid := q[len(q)-1]
				q = q[:len(q)-1]
				if _, seen := havesReachable[oid]; seen {
					continue
				}
				obj, ok := repo.Objects[oid]
				if !ok {
					continue
				}
				havesReachable[oid] = struct{}{}
				switch obj.Type {
				case ObjCommit:
					pc := parseCommit(obj.Data)
					q = append(q, pc.Tree)
					if _, stop := stopAt[oid]; !stop {
						q = append(q, pc.Parents...)
					}
				case ObjTree:
					for _, e := range parseTree(obj.Data) {
						if e.Mode != "160000" {
							q = append(q, e.OID)
						}
					}
				case ObjTag:
					pt := parseTag(obj.Data)
					if pt.Object != "" {
						q = append(q, pt.Object)
					}
				}
			}
		}

		var objectsToSend []*GitObject
		var newShallows []string
		var unshallows []string

		if hasDepth && depth > 0 {
			reachable, shallowList := collectShallowReachableLocked(repo, wants, depth)
			newShallows = shallowList
			shallowSet := make(map[string]struct{}, len(newShallows))
			for _, s := range newShallows {
				shallowSet[s] = struct{}{}
			}
			for _, cs := range clientShallows {
				if _, stillShallow := shallowSet[cs]; !stillShallow {
					if _, inReach := reachable[cs]; inReach {
						unshallows = append(unshallows, cs)
					}
				}
			}
			oids := make([]string, 0, len(reachable))
			for oid := range reachable {
				if _, have := havesReachable[oid]; !have {
					oids = append(oids, oid)
				}
			}
			sort.Strings(oids)
			objectsToSend = make([]*GitObject, 0, len(oids))
			for _, oid := range oids {
				if obj, ok := repo.Objects[oid]; ok {
					objectsToSend = append(objectsToSend, obj)
				}
			}
		} else {
			reachable := collectReachableLocked(repo, wants)
			oids := make([]string, 0, len(reachable))
			for oid := range reachable {
				if _, have := havesReachable[oid]; !have {
					oids = append(oids, oid)
				}
			}
			sort.Strings(oids)
			objectsToSend = make([]*GitObject, 0, len(oids))
			for _, oid := range oids {
				if obj, ok := repo.Objects[oid]; ok {
					objectsToSend = append(objectsToSend, obj)
				}
			}
		}

		var packBytes []byte
		if isDepth1Cacheable {
			// Level-9 zlib recompression for depth-1 cached pack shrinks wire size and client SHA-1 verification cost
			packBytes = writePackLevel9(objectsToSend)
		} else {
			packBytes = writePack(objectsToSend)
		}

		var body []byte
		if !isV0 {
			if len(haves) > 0 {
				body = appendPktLineStr(body, "acknowledgments\n")
				body = appendPktLineStr(body, "ready\n")
				body = appendPktDelim(body)
			}
			if len(newShallows) > 0 || len(unshallows) > 0 {
				body = appendPktLineStr(body, "shallow-info\n")
				for _, s := range newShallows {
					body = appendPktLineStr(body, "shallow "+s+"\n")
				}
				for _, u := range unshallows {
					body = appendPktLineStr(body, "unshallow "+u+"\n")
				}
				body = appendPktDelim(body)
			}
			body = appendPktLineStr(body, "packfile\n")
		} else {
			if len(newShallows) > 0 || len(unshallows) > 0 {
				for _, s := range newShallows {
					body = appendPktLineStr(body, "shallow "+s+"\n")
				}
				for _, u := range unshallows {
					body = appendPktLineStr(body, "unshallow "+u+"\n")
				}
				body = appendPktFlush(body)
			}
			body = appendPktLineStr(body, "NAK\n")
		}

		const chunkSize = 65515
		for off := 0; off < len(packBytes); off += chunkSize {
			end := off + chunkSize
			if end > len(packBytes) {
				end = len(packBytes)
			}
			chunk := packBytes[off:end]
			n := len(chunk) + 5
			const hexDigits = "0123456789abcdef"
			body = append(body,
				hexDigits[(n>>12)&0xf],
				hexDigits[(n>>8)&0xf],
				hexDigits[(n>>4)&0xf],
				hexDigits[n&0xf],
				1,
			)
			body = append(body, chunk...)
		}
		body = appendPktFlush(body)

		resp := buildHTTPGitFrame("application/x-git-upload-pack-result", body)
		if isDepth1Cacheable && len(wants[0]) == 40 {
			var entry cloneCacheEntry
			copy(entry.tipOID[:], wants[0])
			entry.httpResp = resp
			repo.cloneDepth1Cache = &entry
		}
		return resp
	}

	return buildHTTPFrame("200 OK", "application/x-git-upload-pack-result", []byte("0000"), "Cache-Control: no-cache\r\n")
}

func collectReachableLocked(repo *StoredRepo, roots []string) map[string]struct{} {
	seen := make(map[string]struct{}, len(roots)*4)
	q := make([]string, len(roots))
	copy(q, roots)
	for len(q) > 0 {
		oid := q[len(q)-1]
		q = q[:len(q)-1]
		if _, ok := seen[oid]; ok {
			continue
		}
		obj, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		seen[oid] = struct{}{}
		switch obj.Type {
		case ObjCommit:
			pc := parseCommit(obj.Data)
			q = append(q, pc.Tree)
			q = append(q, pc.Parents...)
		case ObjTree:
			for _, e := range parseTree(obj.Data) {
				if e.Mode != "160000" {
					q = append(q, e.OID)
				}
			}
		case ObjTag:
			pt := parseTag(obj.Data)
			if pt.Object != "" {
				q = append(q, pt.Object)
			}
		}
	}
	return seen
}

func collectShallowReachableLocked(repo *StoredRepo, roots []string, maxDepth int) (map[string]struct{}, []string) {
	objs := make(map[string]struct{}, 16)
	shallowSet := make(map[string]struct{}, 4)

	type item struct {
		oid   string
		depth int
	}
	queue := make([]item, 0, len(roots)*2)
	for _, r := range roots {
		queue = append(queue, item{oid: r, depth: 1})
	}
	visitedCommit := make(map[string]int)

	var collectTree func(oid string)
	collectTree = func(oid string) {
		if _, ok := objs[oid]; ok {
			return
		}
		obj, ok := repo.Objects[oid]
		if !ok {
			return
		}
		objs[oid] = struct{}{}
		if obj.Type == ObjTree {
			for _, e := range parseTree(obj.Data) {
				if e.Mode != "160000" {
					collectTree(e.OID)
				}
			}
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		obj, ok := repo.Objects[cur.oid]
		if !ok {
			continue
		}
		if obj.Type == ObjTag {
			objs[cur.oid] = struct{}{}
			pt := parseTag(obj.Data)
			if pt.Object != "" {
				queue = append(queue, item{oid: pt.Object, depth: cur.depth})
			}
			continue
		}
		if obj.Type == ObjCommit {
			if prevD, visited := visitedCommit[cur.oid]; visited && prevD <= cur.depth {
				continue
			}
			visitedCommit[cur.oid] = cur.depth
			objs[cur.oid] = struct{}{}
			pc := parseCommit(obj.Data)
			collectTree(pc.Tree)
			if cur.depth >= maxDepth {
				if len(pc.Parents) > 0 {
					shallowSet[cur.oid] = struct{}{}
				}
			} else {
				for _, p := range pc.Parents {
					queue = append(queue, item{oid: p, depth: cur.depth + 1})
				}
			}
		} else {
			collectTree(cur.oid)
		}
	}

	shallowList := make([]string, 0, len(shallowSet))
	for s := range shallowSet {
		shallowList = append(shallowList, s)
	}
	sort.Strings(shallowList)
	return objs, shallowList
}

func handleGitReceivePackBytes(store *ForgeStore, owner, repoName, rawBody, wbuf []byte) []byte {
	repo := store.GetRepoBytes(owner, repoName)
	if repo == nil {
		return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
	}
	return handleGitReceivePackRepo(repo, rawBody, wbuf)
}

func handleGitReceivePackRepo(repo *StoredRepo, rawBody, wbuf []byte) []byte {
	// Zero-Alloc Single-Thread Push Fast Path for single-command refs/heads/main fast-forward updates
	if len(rawBody) > 95 {
		if n, err := parseHex4(rawBody[:4]); err == nil && n > 85 && n+4 <= len(rawBody) {
			if rawBody[n] == '0' && rawBody[n+1] == '0' && rawBody[n+2] == '0' && rawBody[n+3] == '0' {
				line := bytes.TrimSuffix(rawBody[4:n], []byte{'\n'})
				var cmdPart, capsPart []byte
				if nul := bytes.IndexByte(line, 0); nul >= 0 {
					cmdPart = line[:nul]
					capsPart = line[nul+1:]
				} else {
					cmdPart = line
				}
				if len(cmdPart) == 82+len(mainRefBytes) && cmdPart[40] == ' ' && cmdPart[81] == ' ' && bytes.Equal(cmdPart[82:], mainRefBytes) {
					oldOIDBytes := cmdPart[:40]
					newOIDBytes := cmdPart[41:81]
					useSideband := bytes.Contains(capsPart, []byte("side-band-64k")) || bytes.Contains(capsPart, []byte("side-band"))
					packData := rawBody[n+4:]

					isDelete := bytes.Equal(newOIDBytes, zeroOIDBytes)
					isCreate := !isDelete && bytes.Equal(oldOIDBytes, zeroOIDBytes)
					if !isDelete && len(packData) > 0 {
						canFast := false
						if isCreate {
							if !repo.hasMainTip && repo.Refs["refs/heads/main"] == "" {
								canFast = true
							}
						} else if repo.hasMainTip && bytes.Equal(repo.mainTip[:], oldOIDBytes) {
							canFast = true
						}
						if canFast {
							fastOK, parentOK, perr := ingestPackFastForwardMain(packData, repo, oldOIDBytes, newOIDBytes)
							if perr == nil && fastOK && parentOK {
								isNewMain := !repo.hasMainTip
								copy(repo.mainTip[:], newOIDBytes)
								repo.hasMainTip = true
								if isNewMain && len(repo.Refs) == 0 {
									repo.onlyMainRef = true
								}
								if !repo.onlyMainRef && len(repo.Refs) > 0 {
									repo.Refs["refs/heads/main"] = string(newOIDBytes)
								}
								repo.lsRefsAllCache = nil
								repo.lsRefsHeadsCache = nil
								repo.cloneDepth1Cache = nil
								if repo.onlyMainRef && len(repo.Refs) == 0 {
									repo.recvAdvCache = buildRecvAdvMainInPlace(repo)
								} else {
									repo.recvAdvCache = buildRecvAdvHTTPLocked(repo)
								}
								if useSideband {
									return respPushMainOKSideband
								}
								var rStack [128]byte
								rb := appendPktLineStr(rStack[:0], "unpack ok\n")
								rb = appendPktLineStr(rb, "ok refs/heads/main\n")
								rb = appendPktFlush(rb)
								wbuf = append(wbuf, "HTTP/1.1 200 OK\r\nContent-Type: application/x-git-receive-pack-result\r\nX-GitHub-Request-Id: gf-1\r\nCache-Control: no-cache\r\nContent-Length: "...)
								wbuf = strconv.AppendInt(wbuf, int64(len(rb)), 10)
								wbuf = append(wbuf, "\r\n\r\n"...)
								wbuf = append(wbuf, rb...)
								return wbuf
							}
						}
					}
				}
			}
		}
	}

	cmdLines, packData, err := decodeReceivePackPkts(rawBody)
	if err != nil {
		return buildHTTPJSONFrame("400 Bad Request", []byte(`{"message":"Malformed pkt-line"}`), "")
	}

	type pushCmd struct {
		oldOID string
		newOID string
		ref    string
	}
	commands := make([]pushCmd, 0, len(cmdLines))
	useSideband := false

	for i, rawLine := range cmdLines {
		line := bytes.TrimSuffix(rawLine, []byte{'\n'})
		var cmdPart []byte
		if i == 0 {
			if nul := bytes.IndexByte(line, 0); nul >= 0 {
				cmdPart = line[:nul]
				capsPart := line[nul+1:]
				if bytes.Contains(capsPart, []byte("side-band-64k")) || bytes.Contains(capsPart, []byte("side-band")) {
					useSideband = true
				}
			} else {
				cmdPart = line
			}
		} else {
			cmdPart = line
		}
		fields := strings.Fields(string(cmdPart))
		if len(fields) >= 3 {
			commands = append(commands, pushCmd{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
		}
	}

	repo.syncMainRefString()
	unpackOk := true
	unpackErr := ""

	if len(packData) > 0 {
		repo.materializePendingPacksLocked()
		if perr := readPackIntoRepo(packData, repo); perr != nil {
			unpackOk = false
			unpackErr = perr.Error()
		}
	}

	type refReport struct {
		ref    string
		ok     bool
		reason string
	}
	reports := make([]refReport, 0, len(commands))
	refsChanged := false

	if unpackOk {
		for _, cmd := range commands {
			currOID := repo.Refs[cmd.ref]
			if cmd.newOID == ZeroOID {
				if currOID != "" && cmd.oldOID != ZeroOID && currOID != cmd.oldOID {
					reports = append(reports, refReport{ref: cmd.ref, ok: false, reason: "stale-info"})
				} else {
					delete(repo.Refs, cmd.ref)
					if cmd.ref == "refs/heads/main" {
						repo.hasMainTip = false
					}
					refsChanged = true
					reports = append(reports, refReport{ref: cmd.ref, ok: true})
				}
			} else if cmd.oldOID == ZeroOID {
				if currOID != "" && currOID != ZeroOID {
					reports = append(reports, refReport{ref: cmd.ref, ok: false, reason: "already exists"})
				} else {
					repo.Refs[cmd.ref] = cmd.newOID
					if cmd.ref == "refs/heads/main" && len(cmd.newOID) == 40 {
						copy(repo.mainTip[:], cmd.newOID)
						repo.hasMainTip = true
					}
					refsChanged = true
					reports = append(reports, refReport{ref: cmd.ref, ok: true})
				}
			} else {
				if currOID != "" && currOID != cmd.oldOID {
					reports = append(reports, refReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				} else if !repo.isAncestorLocked(cmd.newOID, cmd.oldOID) {
					reports = append(reports, refReport{ref: cmd.ref, ok: false, reason: "non-fast-forward"})
				} else {
					repo.Refs[cmd.ref] = cmd.newOID
					if cmd.ref == "refs/heads/main" && len(cmd.newOID) == 40 {
						copy(repo.mainTip[:], cmd.newOID)
						repo.hasMainTip = true
					}
					refsChanged = true
					reports = append(reports, refReport{ref: cmd.ref, ok: true})
				}
			}
		}
	}

	if refsChanged {
		repo.recomputeOnlyMainRef()
		repo.invalidateGitCaches()
		repo.recvAdvCache = buildRecvAdvHTTPLocked(repo)
	}

	var reportBuf []byte
	if unpackOk {
		reportBuf = appendPktLineStr(reportBuf, "unpack ok\n")
		for _, rep := range reports {
			if rep.ok {
				reportBuf = appendPktLineStr(reportBuf, "ok "+rep.ref+"\n")
			} else {
				reportBuf = appendPktLineStr(reportBuf, "ng "+rep.ref+" "+rep.reason+"\n")
			}
		}
	} else {
		reportBuf = appendPktLineStr(reportBuf, "unpack "+unpackErr+"\n")
		for _, cmd := range commands {
			reportBuf = appendPktLineStr(reportBuf, "ng "+cmd.ref+" unpack failed\n")
		}
	}
	reportBuf = appendPktFlush(reportBuf)

	var respBody []byte
	if useSideband {
		n := len(reportBuf) + 5
		const hexDigits = "0123456789abcdef"
		respBody = make([]byte, 0, n+4)
		respBody = append(respBody,
			hexDigits[(n>>12)&0xf],
			hexDigits[(n>>8)&0xf],
			hexDigits[(n>>4)&0xf],
			hexDigits[n&0xf],
			1,
		)
		respBody = append(respBody, reportBuf...)
		respBody = appendPktFlush(respBody)
	} else {
		respBody = reportBuf
	}

	wbuf = append(wbuf, "HTTP/1.1 200 OK\r\nContent-Type: application/x-git-receive-pack-result\r\nX-GitHub-Request-Id: gf-1\r\nCache-Control: no-cache\r\nContent-Length: "...)
	wbuf = strconv.AppendInt(wbuf, int64(len(respBody)), 10)
	wbuf = append(wbuf, "\r\n\r\n"...)
	wbuf = append(wbuf, respBody...)
	return wbuf
}

func firstWord(s string) string {
	if sp := strings.IndexByte(s, ' '); sp >= 0 {
		return s[:sp]
	}
	return s
}

func extractQueryParam(query, key string) string {
	for query != "" {
		var pair string
		if amp := strings.IndexByte(query, '&'); amp >= 0 {
			pair = query[:amp]
			query = query[amp+1:]
		} else {
			pair = query
			query = ""
		}
		if eq := strings.IndexByte(pair, '='); eq >= 0 {
			if pair[:eq] == key {
				return pair[eq+1:]
			}
		}
	}
	return ""
}
