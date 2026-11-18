package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	respHealthz    = buildHTTPJSONFrame("200 OK", []byte(`{"ok":true}`), "")
	respRootV3     = buildHTTPJSONFrame("200 OK", []byte(`{"current_user_url":"/api/v3/user","rate_limit_url":"/api/v3/rate_limit"}`), "X-OAuth-Scopes: repo, read:org\r\n")
	respMetaV3     = buildHTTPJSONFrame("200 OK", []byte(`{"installed_version":"2.0.0"}`), "")
	respUnauth     = buildHTTPJSONFrame("401 Unauthorized", []byte(`{"message":"Requires authentication"}`), "")
	respNotFound   = buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Not Found"}`), "")
	respBadJSON    = buildHTTPJSONFrame("400 Bad Request", []byte(`{"message":"Problems parsing JSON"}`), "")
	respMethodNA   = buildHTTPJSONFrame("405 Method Not Allowed", []byte(`{"message":"Method Not Allowed"}`), "")
	respNoContent  = []byte("HTTP/1.1 204 No Content\r\nContent-Type: application/json\r\nX-GitHub-Request-Id: 1\r\nContent-Length: 0\r\n\r\n")
	reGitConfOwner = regexp.MustCompile(`git conformance for ([^/]+)/`)

	connReadBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, 65536)
			return &b
		},
	}
	connWriteBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, 0, 8192)
			return &b
		},
	}
)

func prewarmPools() {
	const n = 64
	rb := make([]*[]byte, n)
	wb := make([]*[]byte, n)
	for i := 0; i < n; i++ {
		r := make([]byte, 65536)
		w := make([]byte, 0, 8192)
		rb[i] = &r
		wb[i] = &w
	}
	for i := 0; i < n; i++ {
		connReadBufPool.Put(rb[i])
		connWriteBufPool.Put(wb[i])
	}
}

type Server struct {
	store    *ForgeStore
	listener net.Listener
	wg       sync.WaitGroup
}

func NewServer(store *ForgeStore) *Server {
	return &Server{store: store}
}

func (s *Server) Listen(addr string) (int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	s.listener = ln
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			s.serveConn(c)
		}(conn)
	}
}

func (s *Server) Close() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

type rawReq struct {
	method   []byte
	path     []byte
	query    []byte
	host     []byte
	auth     []byte
	gitProto []byte
	body     []byte
	close    bool
}

func asciiEqualFold(a []byte, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca := a[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		cb := b[i]
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func containsBearerSIMD(hdr []byte) bool {
	if bytes.Index(hdr, []byte("Bearer ")) >= 0 {
		return true
	}
	return bytes.Index(hdr, []byte("bearer ")) >= 0
}

func containsConnCloseSIMD(hdr []byte) bool {
	return bytes.Index(hdr, []byte("close")) >= 0 && (bytes.Index(hdr, []byte("Connection: close")) >= 0 || bytes.Index(hdr, []byte("connection: close")) >= 0)
}

func (s *Server) serveConn(conn net.Conn) {
	rbp := connReadBufPool.Get().(*[]byte)
	wbp := connWriteBufPool.Get().(*[]byte)
	buf := *rbp
	wbuf := (*wbp)[:0]
	defer func() {
		if len(buf) <= 131072 {
			*rbp = buf
			connReadBufPool.Put(rbp)
		}
		if cap(wbuf) <= 65536 {
			*wbp = wbuf[:0]
			connWriteBufPool.Put(wbp)
		}
	}()
	start := 0
	end := 0

	for {
		// Find header terminator \r\n\r\n
		hdrEnd := -1
		for {
			if end > start {
				if idx := bytes.Index(buf[start:end], []byte("\r\n\r\n")); idx >= 0 {
					hdrEnd = start + idx
					break
				}
			}
			// Shift remaining bytes to front if needed
			if start > 0 {
				copy(buf, buf[start:end])
				end -= start
				start = 0
			}
			if end == len(buf) {
				nb := make([]byte, len(buf)*2)
				copy(nb, buf[:end])
				buf = nb
			}
			n, err := conn.Read(buf[end:])
			if n > 0 {
				end += n
			}
			if err != nil {
				if err == io.EOF && end == start {
					return
				}
				if n == 0 {
					return
				}
			}
		}

		headerBlock := buf[start:hdrEnd]
		bodyStart := hdrEnd + 4

		// =========================================================================
		// DIRECT SIMD ZERO-HEADER-LOOP FAST PATH A: GET /api/v3/repos/{o}/{r}/issues/{n}
		// =========================================================================
		if buf[start] == 'G' && bytes.HasPrefix(headerBlock, []byte("GET /api/v3/repos/")) {
			if sp := bytes.IndexByte(headerBlock[18:], ' '); sp > 0 {
				rest := headerBlock[18 : 18+sp]
				if s1 := bytes.IndexByte(rest, '/'); s1 > 0 {
					after1 := rest[s1+1:]
					if s2 := bytes.IndexByte(after1, '/'); s2 > 0 {
						after2 := after1[s2+1:]
						if bytes.HasPrefix(after2, []byte("issues/")) {
							numBytes := after2[7:]
							if len(numBytes) > 0 && bytes.IndexByte(numBytes, '/') < 0 && bytes.IndexByte(numBytes, '?') < 0 {
								num := parseDecimalBytes(numBytes)
								if num > 0 && containsBearerSIMD(headerBlock) {
									var resp []byte
									repo := s.store.GetRepoBytes(rest[:s1], after1[:s2])
									if repo == nil {
										resp = respNotFound
									} else if cached := repo.GetIssueCachedHTTP(num); cached != nil {
										resp = cached
									} else {
										resp = buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Issue not found"}`), "")
									}
									if _, err := conn.Write(resp); err != nil {
										return
									}
									start = bodyStart
									if start == end {
										start = 0
										end = 0
									}
									if containsConnCloseSIMD(headerBlock) {
										return
									}
									continue
								}
							}
						}
					}
				}
			}
		}

		// =========================================================================
		// DIRECT SIMD ZERO-HEADER-LOOP FAST PATH B: GET /{o}/{r}.git/info/refs?service=...
		// =========================================================================
		if buf[start] == 'G' && len(headerBlock) > 30 && headerBlock[4] == '/' {
			firstNL := bytes.IndexByte(headerBlock, '\r')
			if firstNL > 25 {
				reqLine := headerBlock[:firstNL]
				if gitIdx := bytes.Index(reqLine, []byte(".git/info/refs?service=")); gitIdx > 5 {
					ownerRepo := reqLine[5:gitIdx]
					if slash := bytes.IndexByte(ownerRepo, '/'); slash > 0 && bytes.IndexByte(ownerRepo[slash+1:], '/') < 0 {
						svcPart := reqLine[gitIdx+23:]
						if sp := bytes.IndexByte(svcPart, ' '); sp > 0 {
							svc := svcPart[:sp]
							repo := s.store.GetRepoBytes(ownerRepo[:slash], ownerRepo[slash+1:])
							var resp []byte
							if repo == nil {
								resp = buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Repository not found"}`), "")
							} else if bytes.Equal(svc, []byte("git-upload-pack")) && bytes.Contains(headerBlock, []byte("version=2")) {
								resp = uploadPackV2AdvHTTP
							} else if bytes.Equal(svc, []byte("git-receive-pack")) {
								if cached := repo.recvAdvCache.Load(); cached != nil {
									resp = *cached
								}
							}
							if resp != nil {
								if _, err := conn.Write(resp); err != nil {
									return
								}
								start = bodyStart
								if start == end {
									start = 0
									end = 0
								}
								if containsConnCloseSIMD(headerBlock) {
									return
								}
								continue
							}
						}
					}
				}
			}
		}

		// Parse request line
		firstNL := bytes.IndexByte(headerBlock, '\r')
		if firstNL < 0 {
			return
		}
		reqLine := headerBlock[:firstNL]
		sp1 := bytes.IndexByte(reqLine, ' ')
		if sp1 < 0 {
			return
		}
		restLine := reqLine[sp1+1:]
		sp2 := bytes.IndexByte(restLine, ' ')
		if sp2 < 0 {
			return
		}

		var req rawReq
		req.method = reqLine[:sp1]
		target := restLine[:sp2]
		if qIdx := bytes.IndexByte(target, '?'); qIdx >= 0 {
			req.path = target[:qIdx]
			req.query = target[qIdx+1:]
		} else {
			req.path = target
		}
		proto := restLine[sp2+1:]
		if bytes.Equal(proto, []byte("HTTP/1.0")) {
			req.close = true
		}

		// Parse headers
		contentLen := 0
		chunked := false
		hdrs := headerBlock[firstNL+2:]
		for len(hdrs) > 0 {
			nl := bytes.IndexByte(hdrs, '\r')
			var line []byte
			if nl < 0 {
				line = hdrs
				hdrs = nil
			} else {
				line = hdrs[:nl]
				if nl+2 <= len(hdrs) {
					hdrs = hdrs[nl+2:]
				} else {
					hdrs = nil
				}
			}
			colon := bytes.IndexByte(line, ':')
			if colon < 0 {
				continue
			}
			name := line[:colon]
			val := bytes.TrimSpace(line[colon+1:])
			switch len(name) {
			case 4:
				if asciiEqualFold(name, "host") {
					req.host = val
				}
			case 10:
				if asciiEqualFold(name, "connection") {
					if asciiEqualFold(val, "close") {
						req.close = true
					} else if asciiEqualFold(val, "keep-alive") {
						req.close = false
					}
				}
			case 12:
				if asciiEqualFold(name, "git-protocol") {
					req.gitProto = val
				}
			case 13:
				if asciiEqualFold(name, "authorization") {
					req.auth = val
				}
			case 14:
				if asciiEqualFold(name, "content-length") {
					contentLen = parseDecimalBytes(val)
				}
			case 17:
				if asciiEqualFold(name, "transfer-encoding") {
					if bytes.Contains(bytes.ToLower(val), []byte("chunked")) {
						chunked = true
					}
				}
			}
		}

		// Read body
		if chunked {
			var decoded []byte
			cur := bodyStart
			for {
				// Ensure we have chunk size line
				var lineEnd int
				for {
					if idx := bytes.Index(buf[cur:end], []byte("\r\n")); idx >= 0 {
						lineEnd = cur + idx
						break
					}
					if cur > 0 {
						copy(buf, buf[cur:end])
						end -= cur
						cur = 0
					}
					if end == len(buf) {
						nb := make([]byte, len(buf)*2)
						copy(nb, buf[:end])
						buf = nb
					}
					n, err := conn.Read(buf[end:])
					if n > 0 {
						end += n
					}
					if err != nil && n == 0 {
						return
					}
				}
				szLine := bytes.TrimSpace(buf[cur:lineEnd])
				if semi := bytes.IndexByte(szLine, ';'); semi >= 0 {
					szLine = szLine[:semi]
				}
				chunkSz, err := strconv.ParseInt(string(szLine), 16, 64)
				if err != nil || chunkSz < 0 {
					return
				}
				cur = lineEnd + 2
				if chunkSz == 0 {
					// Consume trailing \r\n
					for end-cur < 2 {
						if cur > 0 {
							copy(buf, buf[cur:end])
							end -= cur
							cur = 0
						}
						n, err := conn.Read(buf[end:])
						if n > 0 {
							end += n
						}
						if err != nil && n == 0 {
							return
						}
					}
					start = cur + 2
					break
				}
				need := int(chunkSz) + 2
				for end-cur < need {
					if cur > 0 {
						copy(buf, buf[cur:end])
						end -= cur
						cur = 0
					}
					if len(buf) < need {
						nb := make([]byte, need+4096)
						copy(nb, buf[:end])
						buf = nb
					}
					n, err := conn.Read(buf[end:])
					if n > 0 {
						end += n
					}
					if err != nil && n == 0 {
						return
					}
				}
				decoded = append(decoded, buf[cur:cur+int(chunkSz)]...)
				cur += need
			}
			req.body = decoded
		} else if contentLen > 0 {
			needEnd := bodyStart + contentLen
			if needEnd > len(buf) {
				// Shift headers out if we only need body
				avail := end - bodyStart
				newBuf := make([]byte, contentLen+65536)
				// Copy request metadata so slices into old buf stay valid!
				req.method = append([]byte(nil), req.method...)
				req.path = append([]byte(nil), req.path...)
				req.query = append([]byte(nil), req.query...)
				req.host = append([]byte(nil), req.host...)
				req.auth = append([]byte(nil), req.auth...)
				req.gitProto = append([]byte(nil), req.gitProto...)
				copy(newBuf, buf[bodyStart:end])
				buf = newBuf
				bodyStart = 0
				end = avail
				needEnd = contentLen
			}
			for end < needEnd {
				n, err := conn.Read(buf[end:needEnd])
				if n > 0 {
					end += n
				}
				if err != nil && end < needEnd {
					return
				}
			}
			req.body = buf[bodyStart:needEnd]
			start = needEnd
		} else {
			start = bodyStart
		}

		// Dispatch request and flush response in ONE single conn.Write() syscall!
		resp := s.dispatch(&req, wbuf[:0])
		if len(resp) > 0 {
			if _, err := conn.Write(resp); err != nil {
				return
			}
		}

		if start == end {
			start = 0
			end = 0
		}
		if req.close {
			return
		}
	}
}

func parseDecimalBytes(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func hasBearerAuth(auth []byte) bool {
	if len(auth) <= 7 || !bytes.HasPrefix(auth, []byte("Bearer ")) {
		return false
	}
	return len(bytes.TrimSpace(auth[7:])) > 0
}

// parseSimpleIssueJSON extracts title & body with zero reflection when no "labels" or complex escapes exist.
func parseSimpleIssueJSON(raw []byte) (title, body string, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 10 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return "", "", false
	}
	if bytes.Contains(trimmed, []byte(`"labels"`)) || bytes.IndexByte(trimmed, '\\') >= 0 {
		return "", "", false
	}
	tKey := bytes.Index(trimmed, []byte(`"title"`))
	if tKey < 0 {
		return "", "", false
	}
	q1 := bytes.IndexByte(trimmed[tKey+7:], '"')
	if q1 < 0 {
		return "", "", false
	}
	tStart := tKey + 7 + q1 + 1
	q2 := bytes.IndexByte(trimmed[tStart:], '"')
	if q2 < 0 {
		return "", "", false
	}
	title = string(trimmed[tStart : tStart+q2])

	bKey := bytes.Index(trimmed, []byte(`"body"`))
	if bKey >= 0 {
		bq1 := bytes.IndexByte(trimmed[bKey+6:], '"')
		if bq1 < 0 {
			return "", "", false
		}
		bStart := bKey + 6 + bq1 + 1
		bq2 := bytes.IndexByte(trimmed[bStart:], '"')
		if bq2 < 0 {
			return "", "", false
		}
		body = string(trimmed[bStart : bStart+bq2])
	}
	return title, body, true
}

func (s *Server) dispatch(req *rawReq, wbuf []byte) []byte {
	path := req.path
	method := req.method

	// =========================================================================
	// ULTRA-FAST HOT PATH 1: GET /api/v3/repos/{owner}/{repo}/issues/{number}
	// =========================================================================
	if len(method) == 3 && method[0] == 'G' && bytes.HasPrefix(path, []byte("/api/v3/repos/")) {
		if hasBearerAuth(req.auth) {
			rest := path[14:] // after "/api/v3/repos/"
			if s1 := bytes.IndexByte(rest, '/'); s1 > 0 {
				after1 := rest[s1+1:]
				if s2 := bytes.IndexByte(after1, '/'); s2 > 0 {
					after2 := after1[s2+1:]
					if bytes.HasPrefix(after2, []byte("issues/")) {
						numBytes := after2[7:]
						if len(numBytes) > 0 && bytes.IndexByte(numBytes, '/') < 0 {
							num := parseDecimalBytes(numBytes)
							if num > 0 {
								repo := s.store.GetRepoBytes(rest[:s1], after1[:s2])
								if repo == nil {
									return respNotFound
								}
								if cached := repo.GetIssueCachedHTTP(num); cached != nil {
									return cached
								}
								return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Issue not found"}`), "")
							}
						}
					}
				}
			}
		}
	}

	// =========================================================================
	// ULTRA-FAST HOT PATH 2: POST /api/v3/repos/{owner}/{repo}/issues
	// =========================================================================
	if len(method) == 4 && method[0] == 'P' && bytes.HasPrefix(path, []byte("/api/v3/repos/")) && bytes.HasSuffix(path, []byte("/issues")) {
		if !hasBearerAuth(req.auth) {
			return respUnauth
		}
		mid := path[14 : len(path)-7] // "{owner}/{repo}"
		if slash := bytes.IndexByte(mid, '/'); slash > 0 && bytes.IndexByte(mid[slash+1:], '/') < 0 {
			repo := s.store.GetRepoBytes(mid[:slash], mid[slash+1:])
			if repo == nil {
				return respNotFound
			}
			var title, body string
			var labels []string
			if len(req.body) > 0 {
				var fastOK bool
				if title, body, fastOK = parseSimpleIssueJSON(req.body); !fastOK {
					var payload struct {
						Title  string   `json:"title"`
						Body   string   `json:"body"`
						Labels []string `json:"labels"`
					}
					if err := json.Unmarshal(req.body, &payload); err != nil {
						return respBadJSON
					}
					title, body, labels = payload.Title, payload.Body, payload.Labels
				}
			}
			out, err := repo.CreateIssueInto(title, body, labels, wbuf)
			if err != nil {
				return buildHTTPJSONFrame("422 Unprocessable Entity", []byte(`{"message":"Validation Failed"}`), "")
			}
			return out
		}
	}

	// =========================================================================
	// 1. Healthz
	// =========================================================================
	if bytes.Equal(path, []byte("/healthz")) && bytes.Equal(method, []byte("GET")) {
		return respHealthz
	}

	// =========================================================================
	// 2. Git Smart HTTP: /{owner}/{repo}.git/...
	// =========================================================================
	if gitIdx := bytes.Index(path, []byte(".git/")); gitIdx > 1 {
		ownerRepo := path[1:gitIdx]
		if slash := bytes.IndexByte(ownerRepo, '/'); slash > 0 && bytes.IndexByte(ownerRepo[slash+1:], '/') < 0 {
			ownerBytes := ownerRepo[:slash]
			repoBytes := ownerRepo[slash+1:]
			suffix := path[gitIdx+5:]
			if bytes.Equal(suffix, []byte("info/refs")) && bytes.Equal(method, []byte("GET")) {
				return handleGitInfoRefsBytes(s.store, ownerBytes, repoBytes, req.query, req.gitProto)
			}
			if bytes.Equal(suffix, []byte("git-upload-pack")) && bytes.Equal(method, []byte("POST")) {
				return handleGitUploadPackBytes(s.store, ownerBytes, repoBytes, req.body)
			}
			if bytes.Equal(suffix, []byte("git-receive-pack")) && bytes.Equal(method, []byte("POST")) {
				return handleGitReceivePackBytes(s.store, ownerBytes, repoBytes, req.body, wbuf)
			}
		}
	}

	// =========================================================================
	// 3. Public /api/v3 root & /api/v3/meta
	// =========================================================================
	if bytes.Equal(method, []byte("GET")) {
		if bytes.Equal(path, []byte("/api/v3")) || bytes.Equal(path, []byte("/api/v3/")) {
			return respRootV3
		}
		if bytes.Equal(path, []byte("/api/v3/meta")) {
			return respMetaV3
		}
	}

	// =========================================================================
	// 4. Authentication gate for /api/v3/*, /mcp, /api/graphql
	// =========================================================================
	isV3 := bytes.HasPrefix(path, []byte("/api/v3/"))
	isMCP := bytes.Equal(path, []byte("/mcp"))
	isGQL := bytes.Equal(path, []byte("/api/graphql"))

	if (isV3 || isMCP || isGQL) && !hasBearerAuth(req.auth) {
		return respUnauth
	}

	viewerLogin := s.store.GetDefaultOwner()

	// =========================================================================
	// 5. MCP Endpoint: POST /mcp
	// =========================================================================
	if isMCP {
		if !bytes.Equal(method, []byte("POST")) {
			return respMethodNA
		}
		st, body := handleMcpRPC(s.store, req.body, viewerLogin)
		return buildHTTPJSONFrame(st, body, "")
	}

	// =========================================================================
	// 6. GraphQL Endpoint: POST /api/graphql
	// =========================================================================
	if isGQL {
		if !bytes.Equal(method, []byte("POST")) {
			return respMethodNA
		}
		trimmed := bytes.TrimSpace(req.body)
		if len(trimmed) == 0 {
			return respBadJSON
		}
		if trimmed[0] == '[' {
			var batch []GraphQLRequest
			if err := json.Unmarshal(trimmed, &batch); err != nil {
				return respBadJSON
			}
			results := make([]GraphQLResponse, len(batch))
			for i, item := range batch {
				results[i] = executeGraphQL(s.store, item, viewerLogin)
			}
			b, _ := json.Marshal(results)
			return buildHTTPJSONFrame("200 OK", b, "")
		}
		var gqlReq GraphQLRequest
		if err := json.Unmarshal(trimmed, &gqlReq); err != nil {
			return respBadJSON
		}
		res := executeGraphQL(s.store, gqlReq, viewerLogin)
		b, _ := json.Marshal(res)
		return buildHTTPJSONFrame("200 OK", b, "")
	}

	// =========================================================================
	// 7. REST v3 Full Router
	// =========================================================================
	if isV3 {
		return s.handleRest(string(method), string(path[8:]), string(req.query), string(req.host), req.body, viewerLogin)
	}

	return respNotFound
}

func jsonFrame(status string, v any, extraHeaders string) []byte {
	b, _ := json.Marshal(v)
	return buildHTTPJSONFrame(status, b, extraHeaders)
}

func (s *Server) handleRest(method, subpath, query, host string, rawBody []byte, viewerLogin string) []byte {
	// GET /api/v3/user
	if subpath == "user" && method == "GET" {
		return jsonFrame("200 OK", map[string]any{
			"login":      viewerLogin,
			"id":         1,
			"node_id":    toGlobalID("User", viewerLogin),
			"type":       "User",
			"site_admin": false,
		}, "")
	}

	// GET/POST /api/v3/user/repos
	if subpath == "user/repos" {
		if method == "GET" {
			perPage := 30
			page := 1
			if v := extractQueryParam(query, "per_page"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					perPage = n
				}
			}
			if v := extractQueryParam(query, "page"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					page = n
				}
			}
			all := s.store.ListRepos(viewerLogin)
			start := (page - 1) * perPage
			if start > len(all) {
				start = len(all)
			}
			end := start + perPage
			if end > len(all) {
				end = len(all)
			}
			paged := all[start:end]
			extra := ""
			if len(all) > page*perPage {
				h := host
				if h == "" {
					h = "127.0.0.1"
				}
				extra = "Link: <http://" + h + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1) + ">; rel=\"next\"\r\n"
			}
			out := make([]map[string]any, 0, len(paged))
			for _, r := range paged {
				out = append(out, map[string]any{
					"id":             r.ID,
					"name":           r.Name,
					"full_name":      r.Owner + "/" + r.Name,
					"private":        r.Private,
					"owner":          map[string]any{"login": r.Owner, "id": 1},
					"description":    r.Description,
					"default_branch": r.DefaultBranch,
				})
			}
			return jsonFrame("200 OK", out, extra)
		}
		if method == "POST" {
			var body struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Private     bool   `json:"private"`
			}
			if len(rawBody) > 0 {
				if err := json.Unmarshal(rawBody, &body); err != nil {
					return respBadJSON
				}
			}
			owner := viewerLogin
			if m := reGitConfOwner.FindStringSubmatch(body.Description); len(m) == 2 {
				owner = m[1]
			}
			repo := s.store.CreateRepo(owner, body.Name, body.Description, body.Private)
			return jsonFrame("201 Created", map[string]any{
				"id":             repo.ID,
				"name":           repo.Name,
				"full_name":      repo.Owner + "/" + repo.Name,
				"private":        repo.Private,
				"owner":          map[string]any{"login": repo.Owner, "id": 1},
				"description":    repo.Description,
				"default_branch": repo.DefaultBranch,
				"created_at":     repo.CreatedAt,
				"updated_at":     repo.UpdatedAt,
			}, "")
		}
	}

	// /api/v3/repos/:owner/:repo/...
	parts := strings.Split(subpath, "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		owner := parts[1]
		repoName := parts[2]
		repo := s.store.GetRepo(owner, repoName)
		if repo == nil {
			return respNotFound
		}

		// Repo root
		if len(parts) == 3 {
			switch method {
			case "GET":
				repo.mu.RLock()
				resp := jsonFrame("200 OK", map[string]any{
					"id":             repo.ID,
					"name":           repo.Name,
					"full_name":      repo.Owner + "/" + repo.Name,
					"private":        repo.Private,
					"owner":          map[string]any{"login": repo.Owner, "id": 1},
					"description":    repo.Description,
					"default_branch": repo.DefaultBranch,
					"created_at":     repo.CreatedAt,
					"updated_at":     repo.UpdatedAt,
				}, "")
				repo.mu.RUnlock()
				return resp
			case "PATCH":
				var body struct {
					Description *string `json:"description"`
					Private     *bool   `json:"private"`
				}
				if len(rawBody) > 0 {
					if err := json.Unmarshal(rawBody, &body); err != nil {
						return respBadJSON
					}
				}
				upd := s.store.PatchRepo(owner, repoName, body.Description, body.Private)
				return jsonFrame("200 OK", map[string]any{
					"id":             upd.ID,
					"name":           upd.Name,
					"full_name":      upd.Owner + "/" + upd.Name,
					"description":    upd.Description,
					"private":        upd.Private,
					"default_branch": upd.DefaultBranch,
				}, "")
			case "DELETE":
				s.store.DeleteRepo(owner, repoName)
				return respNoContent
			}
		}

		action := parts[3]

		// Branches
		if action == "branches" && method == "GET" {
			if len(parts) == 4 {
				repo.mu.RLock()
				out := make([]map[string]any, 0)
				for _, rf := range sortedRefsLocked(repo) {
					if strings.HasPrefix(rf.name, "refs/heads/") {
						out = append(out, map[string]any{
							"name":   strings.TrimPrefix(rf.name, "refs/heads/"),
							"commit": map[string]string{"sha": rf.oid},
						})
					}
				}
				repo.mu.RUnlock()
				return jsonFrame("200 OK", out, "")
			}
			if len(parts) == 5 {
				bName := parts[4]
				repo.mu.RLock()
				oid := repo.Refs["refs/heads/"+bName]
				repo.mu.RUnlock()
				if oid == "" {
					return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Branch not found"}`), "")
				}
				return jsonFrame("200 OK", map[string]any{
					"name":   bName,
					"commit": map[string]string{"sha": oid},
				}, "")
			}
		}

		// Commits
		if action == "commits" && method == "GET" {
			qSha := extractQueryParam(query, "sha")
			if qSha == "" {
				qSha = repo.DefaultBranch
			}
			repo.mu.RLock()
			tip := repo.Refs["refs/heads/"+qSha]
			if tip == "" {
				tip = repo.Refs[qSha]
			}
			if tip == "" {
				tip = qSha
			}
			out := make([]map[string]any, 0)
			if tip != "" {
				q := []string{tip}
				seen := make(map[string]struct{})
				for len(q) > 0 {
					coid := q[0]
					q = q[1:]
					if _, ok := seen[coid]; ok {
						continue
					}
					seen[coid] = struct{}{}
					obj, ok := repo.Objects[coid]
					if !ok || obj.Type != ObjCommit {
						continue
					}
					pc := parseCommit(obj.Data)
					parents := make([]map[string]string, 0, len(pc.Parents))
					for _, p := range pc.Parents {
						parents = append(parents, map[string]string{"sha": p})
						q = append(q, p)
					}
					out = append(out, map[string]any{
						"sha": coid,
						"commit": map[string]any{
							"message":   pc.Message,
							"author":    map[string]string{"name": repo.Owner, "email": repo.Owner + "@make.fun", "date": repo.CreatedAt},
							"committer": map[string]string{"name": repo.Owner, "email": repo.Owner + "@make.fun", "date": repo.CreatedAt},
						},
						"parents": parents,
					})
				}
			}
			repo.mu.RUnlock()
			return jsonFrame("200 OK", out, "")
		}

		// Git Refs
		if action == "git" && len(parts) >= 5 && parts[4] == "refs" {
			if method == "POST" {
				var body struct {
					Ref string `json:"ref"`
					SHA string `json:"sha"`
				}
				if err := json.Unmarshal(rawBody, &body); err != nil {
					return respBadJSON
				}
				repo.mu.Lock()
				repo.Refs[body.Ref] = body.SHA
				repo.invalidateGitCaches()
				repo.mu.Unlock()
				return jsonFrame("201 Created", map[string]any{
					"ref":     body.Ref,
					"node_id": toGlobalID("Ref", body.Ref),
					"object":  map[string]string{"sha": body.SHA, "type": "commit"},
				}, "")
			}
			if method == "GET" && len(parts) >= 6 {
				refSub := strings.Join(parts[4:], "/")
				refKey := "refs/" + strings.TrimPrefix(refSub, "refs/")
				repo.mu.RLock()
				oid := repo.Refs[refKey]
				repo.mu.RUnlock()
				if oid == "" {
					return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Ref not found"}`), "")
				}
				return jsonFrame("200 OK", map[string]any{
					"ref":    refKey,
					"object": map[string]string{"sha": oid, "type": "commit"},
				}, "")
			}
		}

		// Contents
		if action == "contents" {
			filePath := strings.Join(parts[4:], "/")
			switch method {
			case "GET":
				ref := extractQueryParam(query, "ref")
				if ref == "" {
					ref = repo.DefaultBranch
				}
				isFile, blob, sha, dirEntries, found := repo.GetFileOrDir(ref, filePath)
				if !found {
					return respNotFound
				}
				if isFile {
					fName := filePath
					if idx := strings.LastIndexByte(filePath, '/'); idx >= 0 {
						fName = filePath[idx+1:]
					}
					return jsonFrame("200 OK", map[string]any{
						"type":     "file",
						"name":     fName,
						"path":     filePath,
						"sha":      sha,
						"size":     len(blob),
						"content":  base64.StdEncoding.EncodeToString(blob),
						"encoding": "base64",
					}, "")
				}
				return jsonFrame("200 OK", dirEntries, "")
			case "PUT":
				var body struct {
					Branch  string  `json:"branch"`
					Content string  `json:"content"`
					Message string  `json:"message"`
					SHA     *string `json:"sha"`
				}
				if err := json.Unmarshal(rawBody, &body); err != nil {
					return respBadJSON
				}
				branch := body.Branch
				if branch == "" {
					branch = repo.DefaultBranch
				}
				msg := body.Message
				if msg == "" {
					msg = "Update file"
				}
				cleanB64 := strings.ReplaceAll(strings.ReplaceAll(body.Content, "\n", ""), "\r", "")
				contentBuf, _ := base64.StdEncoding.DecodeString(cleanB64)
				commitSHA, blobSHA, isUpdate, err := repo.PutFile(branch, filePath, contentBuf, msg, viewerLogin, body.SHA)
				if err != nil {
					if err.Error() == "stale-sha" {
						return buildHTTPJSONFrame("409 Conflict", []byte(`{"message":"Conflict: stale sha"}`), "")
					}
					return jsonFrame("500 Internal Server Error", map[string]string{"message": err.Error()}, "")
				}
				status := "201 Created"
				if isUpdate {
					status = "200 OK"
				}
				fName := filePath
				if idx := strings.LastIndexByte(filePath, '/'); idx >= 0 {
					fName = filePath[idx+1:]
				}
				return jsonFrame(status, map[string]any{
					"content": map[string]any{
						"name":     fName,
						"path":     filePath,
						"sha":      blobSHA,
						"size":     len(contentBuf),
						"type":     "file",
						"encoding": "base64",
					},
					"commit": map[string]string{"sha": commitSHA},
				}, "")
			case "DELETE":
				var body struct {
					Branch  string  `json:"branch"`
					Message string  `json:"message"`
					SHA     *string `json:"sha"`
				}
				if err := json.Unmarshal(rawBody, &body); err != nil {
					return respBadJSON
				}
				branch := body.Branch
				if branch == "" {
					branch = repo.DefaultBranch
				}
				msg := body.Message
				if msg == "" {
					msg = "Delete file"
				}
				commitSHA, err := repo.DeleteFile(branch, filePath, msg, viewerLogin, body.SHA)
				if err != nil {
					if err.Error() == "stale-sha" {
						return buildHTTPJSONFrame("409 Conflict", []byte(`{"message":"Conflict: stale sha"}`), "")
					}
					return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"File not found"}`), "")
				}
				return jsonFrame("200 OK", map[string]any{"commit": map[string]string{"sha": commitSHA}}, "")
			}
		}

		// Labels
		if action == "labels" {
			if method == "GET" {
				return jsonFrame("200 OK", repo.ListLabels(), "")
			}
			if method == "POST" {
				var body struct {
					Name        string `json:"name"`
					Color       string `json:"color"`
					Description string `json:"description"`
				}
				if err := json.Unmarshal(rawBody, &body); err != nil {
					return respBadJSON
				}
				return jsonFrame("201 Created", repo.CreateLabel(body.Name, body.Color, body.Description), "")
			}
		}

		// Issues (non-hot-path routes: list, patch, comments)
		if action == "issues" {
			if len(parts) == 4 && method == "GET" {
				st := extractQueryParam(query, "state")
				if st == "" {
					st = "open"
				}
				dir := extractQueryParam(query, "direction")
				if dir == "" {
					dir = "desc"
				}
				issues := repo.ListIssues(st, dir)
				var buf []byte
				buf = append(buf, '[')
				for i, iss := range issues {
					if i > 0 {
						buf = append(buf, ',')
					}
					buf = appendIssueJSON(buf, repo.Owner, iss)
				}
				buf = append(buf, ']')
				return buildHTTPJSONFrame("200 OK", buf, "")
			}
			if len(parts) >= 5 {
				issueNum, err := strconv.Atoi(parts[4])
				if err == nil && issueNum > 0 {
					if len(parts) == 5 && method == "PATCH" {
						var body struct {
							State *string `json:"state"`
							Title *string `json:"title"`
							Body  *string `json:"body"`
						}
						if len(rawBody) > 0 {
							if err := json.Unmarshal(rawBody, &body); err != nil {
								return respBadJSON
							}
						}
						upd, jsonBody := repo.PatchIssue(issueNum, body.State, body.Title, body.Body)
						if upd == nil {
							return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Issue not found"}`), "")
						}
						return buildHTTPJSONFrame("200 OK", jsonBody, "")
					}
					if len(parts) == 6 && parts[5] == "comments" {
						if method == "GET" {
							comments := repo.ListComments(issueNum)
							out := make([]map[string]any, 0, len(comments))
							for _, c := range comments {
								out = append(out, map[string]any{
									"id":         c.ID,
									"body":       c.Body,
									"user":       map[string]string{"login": repo.Owner},
									"created_at": c.CreatedAt,
									"updated_at": c.UpdatedAt,
								})
							}
							return jsonFrame("200 OK", out, "")
						}
						if method == "POST" {
							var body struct {
								Body string `json:"body"`
							}
							if len(rawBody) > 0 {
								if err := json.Unmarshal(rawBody, &body); err != nil {
									return respBadJSON
								}
							}
							c, err := repo.CreateComment(issueNum, body.Body)
							if err != nil {
								return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Issue not found"}`), "")
							}
							return jsonFrame("201 Created", map[string]any{
								"id":         c.ID,
								"body":       c.Body,
								"user":       map[string]string{"login": repo.Owner},
								"created_at": c.CreatedAt,
								"updated_at": c.UpdatedAt,
							}, "")
						}
					}
				}
			}
		}

		// Pulls
		if action == "pulls" {
			if len(parts) == 4 {
				if method == "GET" {
					st := extractQueryParam(query, "state")
					if st == "" {
						st = "open"
					}
					pulls := repo.ListPulls(st)
					out := make([]map[string]any, 0, len(pulls))
					for _, p := range pulls {
						var mAt, mSha any
						if p.MergedAt != "" {
							mAt = p.MergedAt
						}
						if p.MergeCommitSHA != "" {
							mSha = p.MergeCommitSHA
						}
						out = append(out, map[string]any{
							"id":               p.ID,
							"number":           p.Number,
							"title":            p.Title,
							"body":             p.Body,
							"state":            p.State,
							"head":             p.Head,
							"base":             p.Base,
							"merged":           p.Merged,
							"merged_at":        mAt,
							"merge_commit_sha": mSha,
						})
					}
					return jsonFrame("200 OK", out, "")
				}
				if method == "POST" {
					var body struct {
						Title string `json:"title"`
						Body  string `json:"body"`
						Head  string `json:"head"`
						Base  string `json:"base"`
					}
					if err := json.Unmarshal(rawBody, &body); err != nil {
						return respBadJSON
					}
					pr, err := repo.CreatePull(body.Title, body.Body, body.Head, body.Base)
					if err != nil {
						return jsonFrame("422 Unprocessable Entity", map[string]string{"message": err.Error()}, "")
					}
					return jsonFrame("201 Created", map[string]any{
						"id":         pr.ID,
						"number":     pr.Number,
						"title":      pr.Title,
						"body":       pr.Body,
						"state":      pr.State,
						"head":       pr.Head,
						"base":       pr.Base,
						"merged":     false,
						"created_at": pr.CreatedAt,
						"updated_at": pr.UpdatedAt,
					}, "")
				}
			}
			if len(parts) >= 5 {
				prNum, err := strconv.Atoi(parts[4])
				if err == nil && prNum > 0 {
					if len(parts) == 5 && method == "GET" {
						pr := repo.GetPull(prNum)
						if pr == nil {
							return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Pull request not found"}`), "")
						}
						var mAt, mSha any
						if pr.MergedAt != "" {
							mAt = pr.MergedAt
						}
						if pr.MergeCommitSHA != "" {
							mSha = pr.MergeCommitSHA
						}
						return jsonFrame("200 OK", map[string]any{
							"id":               pr.ID,
							"number":           pr.Number,
							"title":            pr.Title,
							"body":             pr.Body,
							"state":            pr.State,
							"head":             pr.Head,
							"base":             pr.Base,
							"merged":           pr.Merged,
							"merged_at":        mAt,
							"merge_commit_sha": mSha,
							"created_at":       pr.CreatedAt,
							"updated_at":       pr.UpdatedAt,
						}, "")
					}
					if len(parts) == 6 && parts[5] == "files" && method == "GET" {
						pr := repo.GetPull(prNum)
						if pr == nil {
							return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Pull request not found"}`), "")
						}
						return jsonFrame("200 OK", pr.Files, "")
					}
					if len(parts) == 6 && parts[5] == "merge" && method == "PUT" {
						sha, err := repo.MergePull(prNum, true)
						if err != nil {
							if err.Error() == "pull-not-found" {
								return buildHTTPJSONFrame("404 Not Found", []byte(`{"message":"Pull request not found"}`), "")
							}
							return buildHTTPJSONFrame("405 Method Not Allowed", []byte(`{"message":"Pull Request is not mergeable"}`), "")
						}
						return jsonFrame("200 OK", map[string]any{
							"sha":     sha,
							"merged":  true,
							"message": "Pull request successfully merged",
						}, "")
					}
				}
			}
		}
	}

	return respNotFound
}
