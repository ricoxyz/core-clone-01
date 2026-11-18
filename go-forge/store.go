package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type StoredLabel struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type StoredIssue struct {
	ID            int
	Number        int
	Title         string
	Body          string
	State         string // "open" or "closed"
	Labels        []string
	CreatedAt     string
	UpdatedAt     string
	ClosedAt      string
	Comments      int
	CachedGetHTTP []byte // Complete HTTP/1.1 200 OK response bytes for zero-alloc GET /issues/:n
}

type StoredComment struct {
	ID          int
	IssueNumber int
	Body        string
	CreatedAt   string
	UpdatedAt   string
}

type PullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

type PullRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type StoredPull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	Head           PullRef
	Base           PullRef
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	CreatedAt      string
	UpdatedAt      string
	Files          []PullFile
}

type cloneCacheEntry struct {
	tipOID   string
	httpResp []byte
}

type StoredRepo struct {
	mu            sync.RWMutex
	ID            int
	Owner         string
	Name          string
	OwnerBytes    []byte
	NameBytes     []byte
	Description   string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string

	Refs     map[string]string
	Objects  map[string]*GitObject
	Labels   map[string]*StoredLabel
	LabelSeq int

	// Slice-backed O(1) issue store (index = number-1)
	Issues    []*StoredIssue
	lastIssue atomic.Pointer[StoredIssue]

	Comments   []*StoredComment
	Pulls      []*StoredPull
	CommentSeq int
	PullSeq    int

	// Lock-free hot caches invalidated on ref changes
	recvAdvCache       atomic.Pointer[[]byte]
	lsRefsAllCache     atomic.Pointer[[]byte]
	lsRefsHeadsCache   atomic.Pointer[[]byte]
	cloneDepth1Cache   atomic.Pointer[cloneCacheEntry]
}

func (r *StoredRepo) invalidateGitCaches() {
	r.recvAdvCache.Store(nil)
	r.lsRefsAllCache.Store(nil)
	r.lsRefsHeadsCache.Store(nil)
	r.cloneDepth1Cache.Store(nil)
}

var (
	cachedTimeSec atomic.Int64
	cachedTimeStr atomic.Pointer[string]
)

func fastNowISO() string {
	sec := time.Now().Unix()
	if cachedTimeSec.Load() == sec {
		if p := cachedTimeStr.Load(); p != nil {
			return *p
		}
	}
	s := time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
	cachedTimeStr.Store(&s)
	cachedTimeSec.Store(sec)
	return s
}

type ForgeStore struct {
	mu           sync.RWMutex
	repos        map[string]*StoredRepo
	repoList     []*StoredRepo // preserves insertion order
	repoSeq      int
	defaultOwner string
	lastRepo     atomic.Pointer[StoredRepo]
}

func NewForgeStore() *ForgeStore {
	return &ForgeStore{
		repos:        make(map[string]*StoredRepo, 64),
		defaultOwner: "adaiversen",
	}
}

func (s *ForgeStore) GetDefaultOwner() string {
	return s.defaultOwner
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// Fast case-insensitive repo key without allocation when already lowercase
func repoKeyLower(owner, name string) string {
	needLower := false
	for i := 0; i < len(owner); i++ {
		if owner[i] >= 'A' && owner[i] <= 'Z' {
			needLower = true
			break
		}
	}
	if !needLower {
		for i := 0; i < len(name); i++ {
			if name[i] >= 'A' && name[i] <= 'Z' {
				needLower = true
				break
			}
		}
	}
	if !needLower {
		return owner + "/" + name
	}
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (s *ForgeStore) CreateRepo(owner, name, desc string, isPrivate bool) *StoredRepo {
	k := repoKey(owner, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.repos[k]; ok {
		return existing
	}
	s.repoSeq++
	now := fastNowISO()
	r := &StoredRepo{
		ID:            s.repoSeq,
		Owner:         owner,
		Name:          name,
		OwnerBytes:    []byte(owner),
		NameBytes:     []byte(name),
		Description:   desc,
		Private:       isPrivate,
		DefaultBranch: "main",
		CreatedAt:     now,
		UpdatedAt:     now,
		Refs:          make(map[string]string, 8),
		Objects:       make(map[string]*GitObject, 64),
		Labels:        make(map[string]*StoredLabel, 8),
	}
	s.repos[k] = r
	s.repoList = append(s.repoList, r)
	s.lastRepo.Store(r)
	return r
}

func (s *ForgeStore) GetRepo(owner, name string) *StoredRepo {
	if lr := s.lastRepo.Load(); lr != nil && lr.Owner == owner && lr.Name == name {
		return lr
	}
	k := repoKeyLower(owner, name)
	s.mu.RLock()
	r := s.repos[k]
	s.mu.RUnlock()
	if r != nil {
		s.lastRepo.Store(r)
	}
	return r
}

// GetRepoBytes looks up a repo directly from raw path byte slices with ZERO allocations on cache hit!
func (s *ForgeStore) GetRepoBytes(owner, name []byte) *StoredRepo {
	if lr := s.lastRepo.Load(); lr != nil && bytes.Equal(lr.OwnerBytes, owner) && bytes.Equal(lr.NameBytes, name) {
		return lr
	}
	return s.GetRepo(string(owner), string(name))
}

func (s *ForgeStore) GetRepoByID(id int) *StoredRepo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repoList {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (s *ForgeStore) ListRepos(owner string) []*StoredRepo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*StoredRepo, 0, len(s.repoList))
	low := strings.ToLower(owner)
	for _, r := range s.repoList {
		if owner == "" || strings.ToLower(r.Owner) == low {
			out = append(out, r)
		}
	}
	return out
}

func (s *ForgeStore) DeleteRepo(owner, name string) bool {
	k := repoKey(owner, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[k]
	if !ok {
		return false
	}
	delete(s.repos, k)
	if s.lastRepo.Load() == r {
		s.lastRepo.Store(nil)
	}
	for i, item := range s.repoList {
		if item == r {
			s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
			break
		}
	}
	return true
}

func (s *ForgeStore) PatchRepo(owner, name string, desc *string, isPrivate *bool) *StoredRepo {
	r := s.GetRepo(owner, name)
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if desc != nil {
		r.Description = *desc
	}
	if isPrivate != nil {
		r.Private = *isPrivate
	}
	r.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	r.mu.Unlock()
	return r
}

// Fast JSON string escaper into byte slice
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		if i > start {
			dst = append(dst, s[start:i]...)
		}
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			const hexDigits = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		}
		start = i + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	dst = append(dst, '"')
	return dst
}

// appendIssueJSON writes a REST v3 issue object with zero reflection allocations.
func appendIssueJSON(dst []byte, owner string, iss *StoredIssue) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, int64(iss.ID), 10)
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, int64(iss.Number), 10)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, iss.Title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, iss.Body)
	dst = append(dst, `,"state":"`...)
	dst = append(dst, iss.State...)
	dst = append(dst, `","user":{"login":`...)
	dst = appendJSONString(dst, owner)
	dst = append(dst, `},"labels":[`...)
	for i, l := range iss.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"name":`...)
		dst = appendJSONString(dst, l)
		dst = append(dst, '}')
	}
	dst = append(dst, `],"comments":`...)
	dst = strconv.AppendInt(dst, int64(iss.Comments), 10)
	dst = append(dst, `,"created_at":"`...)
	dst = append(dst, iss.CreatedAt...)
	dst = append(dst, `","updated_at":"`...)
	dst = append(dst, iss.UpdatedAt...)
	dst = append(dst, `","closed_at":`...)
	if iss.ClosedAt == "" {
		dst = append(dst, "null"...)
	} else {
		dst = append(dst, '"')
		dst = append(dst, iss.ClosedAt...)
		dst = append(dst, '"')
	}
	dst = append(dst, '}')
	return dst
}

// buildHTTPResponse wraps a JSON body into a single contiguous HTTP/1.1 response frame.
func buildHTTPJSONFrame(statusLine string, body []byte, extraHeaders string) []byte {
	out := make([]byte, 0, len(statusLine)+128+len(extraHeaders)+len(body))
	out = append(out, "HTTP/1.1 "...)
	out = append(out, statusLine...)
	out = append(out, "\r\nContent-Type: application/json; charset=utf-8\r\nX-GitHub-Request-Id: gf-1\r\n"...)
	if extraHeaders != "" {
		out = append(out, extraHeaders...)
	}
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	return out
}

func buildHTTPFrame(statusLine, contentType string, body []byte, extraHeaders string) []byte {
	out := make([]byte, 0, len(statusLine)+128+len(contentType)+len(extraHeaders)+len(body))
	out = append(out, "HTTP/1.1 "...)
	out = append(out, statusLine...)
	out = append(out, "\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nX-GitHub-Request-Id: gf-1\r\n"...)
	if extraHeaders != "" {
		out = append(out, extraHeaders...)
	}
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	return out
}

func (r *StoredRepo) refreshIssueCacheLocked(iss *StoredIssue) []byte {
	body := appendIssueJSON(make([]byte, 0, 256+len(iss.Title)+len(iss.Body)), r.Owner, iss)
	iss.CachedGetHTTP = buildHTTPJSONFrame("200 OK", body, "")
	return body
}

// CreateIssue creates an issue and returns both the issue and its pre-built JSON body.
func (r *StoredRepo) CreateIssue(title, body string, labels []string) (*StoredIssue, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range labels {
		if _, ok := r.Labels[strings.ToLower(l)]; !ok {
			return nil, nil, errors.New("bad-label")
		}
	}
	num := len(r.Issues) + 1
	now := fastNowISO()
	var lblCopy []string
	if len(labels) > 0 {
		lblCopy = make([]string, len(labels))
		copy(lblCopy, labels)
	}
	iss := &StoredIssue{
		ID:        num,
		Number:    num,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    lblCopy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	jsonBody := r.refreshIssueCacheLocked(iss)
	r.Issues = append(r.Issues, iss)
	r.lastIssue.Store(iss)
	return iss, jsonBody, nil
}

// GetIssueCachedHTTP returns the pre-framed HTTP/1.1 200 OK bytes for GET /issues/:num lock-free!
func (r *StoredRepo) GetIssueCachedHTTP(num int) []byte {
	if li := r.lastIssue.Load(); li != nil && li.Number == num {
		return li.CachedGetHTTP
	}
	r.mu.RLock()
	if num < 1 || num > len(r.Issues) {
		r.mu.RUnlock()
		return nil
	}
	iss := r.Issues[num-1]
	resp := iss.CachedGetHTTP
	r.mu.RUnlock()
	r.lastIssue.Store(iss)
	return resp
}

func (r *StoredRepo) PatchIssue(num int, state, title, body *string) (*StoredIssue, []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if num < 1 || num > len(r.Issues) {
		return nil, nil
	}
	iss := r.Issues[num-1]
	now := fastNowISO()
	if state != nil && *state != "" {
		iss.State = *state
		if *state == "closed" {
			iss.ClosedAt = now
		} else {
			iss.ClosedAt = ""
		}
	}
	if title != nil {
		iss.Title = *title
	}
	if body != nil {
		iss.Body = *body
	}
	iss.UpdatedAt = now
	jsonBody := r.refreshIssueCacheLocked(iss)
	r.lastIssue.Store(iss)
	return iss, jsonBody
}

func (r *StoredRepo) ListIssues(state, direction string) []*StoredIssue {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*StoredIssue, 0, len(r.Issues))
	for _, iss := range r.Issues {
		if state == "all" || iss.State == state {
			out = append(out, iss)
		}
	}
	if direction != "asc" {
		// Reverse (Issues slice is already sorted ascending by ID)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func (r *StoredRepo) CreateComment(issueNum int, body string) (*StoredComment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if issueNum < 1 || issueNum > len(r.Issues) {
		return nil, errors.New("issue-not-found")
	}
	iss := r.Issues[issueNum-1]
	r.CommentSeq++
	iss.Comments++
	r.refreshIssueCacheLocked(iss)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	c := &StoredComment{
		ID:          r.CommentSeq,
		IssueNumber: issueNum,
		Body:        body,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	r.Comments = append(r.Comments, c)
	return c, nil
}

func (r *StoredRepo) ListComments(issueNum int) []*StoredComment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*StoredComment, 0)
	for _, c := range r.Comments {
		if c.IssueNumber == issueNum {
			out = append(out, c)
		}
	}
	return out
}

func (r *StoredRepo) CreateLabel(name, color, desc string) *StoredLabel {
	r.mu.Lock()
	defer r.mu.Unlock()
	if color == "" {
		color = "ededed"
	}
	r.LabelSeq++
	lbl := &StoredLabel{
		ID:          r.LabelSeq,
		Name:        name,
		Color:       color,
		Description: desc,
	}
	r.Labels[strings.ToLower(name)] = lbl
	return lbl
}

func (r *StoredRepo) ListLabels() []*StoredLabel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*StoredLabel, 0, len(r.Labels))
	for _, l := range r.Labels {
		out = append(out, l)
	}
	return out
}

// Git object & tree helpers (caller holds r.mu)
func (r *StoredRepo) putObjectLocked(t GitObjType, data []byte) string {
	oid := hashObject(t, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &GitObject{Type: t, Data: data}
	}
	return oid
}

type fileRef struct {
	OID  string
	Mode string
}

func (r *StoredRepo) getTreeEntriesLocked(treeOID, prefix string, out map[string]fileRef) {
	obj, ok := r.Objects[treeOID]
	if !ok || obj.Type != ObjTree {
		return
	}
	for _, e := range parseTree(obj.Data) {
		full := e.Name
		if prefix != "" {
			full = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			r.getTreeEntriesLocked(e.OID, full, out)
		} else {
			out[full] = fileRef{OID: e.OID, Mode: e.Mode}
		}
	}
}

type dirNode struct {
	files map[string]fileRef
	dirs  map[string]*dirNode
}

func newDirNode() *dirNode {
	return &dirNode{files: make(map[string]fileRef), dirs: make(map[string]*dirNode)}
}

func (r *StoredRepo) buildTreeFromFilesLocked(files map[string]fileRef) string {
	root := newDirNode()
	for fp, info := range files {
		parts := strings.Split(fp, "/")
		curr := root
		for i := 0; i < len(parts)-1; i++ {
			seg := parts[i]
			if seg == "" {
				continue
			}
			child, ok := curr.dirs[seg]
			if !ok {
				child = newDirNode()
				curr.dirs[seg] = child
			}
			curr = child
		}
		curr.files[parts[len(parts)-1]] = info
	}
	var save func(*dirNode) string
	save = func(n *dirNode) string {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, info := range n.files {
			entries = append(entries, TreeEntry{Mode: info.Mode, Name: name, OID: info.OID})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: save(child)})
		}
		return r.putObjectLocked(ObjTree, buildTree(entries))
	}
	return save(root)
}

type DirContentEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
	Type string `json:"type"`
}

func (r *StoredRepo) GetFileOrDir(refOrCommit, targetPath string) (isFile bool, blob []byte, sha string, dirEntries []DirContentEntry, found bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	commitOID := r.Refs[refOrCommit]
	if commitOID == "" {
		commitOID = r.Refs["refs/heads/"+refOrCommit]
	}
	if commitOID == "" && isObjectID(refOrCommit) {
		commitOID = refOrCommit
	}
	if commitOID == "" {
		commitOID = r.Refs["refs/heads/"+r.DefaultBranch]
	}
	if commitOID == "" {
		return false, nil, "", nil, false
	}
	cObj, ok := r.Objects[commitOID]
	if !ok || cObj.Type != ObjCommit {
		return false, nil, "", nil, false
	}
	c := parseCommit(cObj.Data)
	clean := strings.Trim(targetPath, "/")
	allFiles := make(map[string]fileRef)
	r.getTreeEntriesLocked(c.Tree, "", allFiles)

	if clean != "" {
		if info, ok := allFiles[clean]; ok {
			if bObj, ok := r.Objects[info.OID]; ok {
				return true, bObj.Data, info.OID, nil, true
			}
			return false, nil, "", nil, false
		}
	}

	prefix := ""
	if clean != "" {
		prefix = clean + "/"
	}
	dirMap := make(map[string]DirContentEntry)
	for p, info := range allFiles {
		if clean == "" || strings.HasPrefix(p, prefix) {
			rel := p
			if clean != "" {
				rel = p[len(prefix):]
			}
			slash := strings.IndexByte(rel, '/')
			if slash >= 0 {
				seg := rel[:slash]
				if _, exists := dirMap[seg]; !exists {
					ep := seg
					if clean != "" {
						ep = clean + "/" + seg
					}
					dirMap[seg] = DirContentEntry{Name: seg, Path: ep, SHA: info.OID, Size: 0, Type: "dir"}
				}
			} else {
				sz := 0
				if bObj, ok := r.Objects[info.OID]; ok {
					sz = len(bObj.Data)
				}
				ep := rel
				if clean != "" {
					ep = clean + "/" + rel
				}
				dirMap[rel] = DirContentEntry{Name: rel, Path: ep, SHA: info.OID, Size: sz, Type: "file"}
			}
		}
	}
	if len(dirMap) > 0 || clean == "" {
		entries := make([]DirContentEntry, 0, len(dirMap))
		for _, v := range dirMap {
			entries = append(entries, v)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		return false, nil, "", entries, true
	}
	return false, nil, "", nil, false
}

func (r *StoredRepo) PutFile(branchName, filePath string, content []byte, message, author string, expectedSHA *string) (commitSHA, blobSHA string, isUpdate bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branchRef := "refs/heads/" + branchName
	currentTip := r.Refs[branchRef]
	clean := strings.Trim(filePath, "/")

	files := make(map[string]fileRef)
	if currentTip != "" {
		if cObj, ok := r.Objects[currentTip]; ok {
			r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", files)
		}
	}
	existing, exists := files[clean]
	if expectedSHA != nil && *expectedSHA != "" && exists && existing.OID != *expectedSHA {
		return "", "", false, errors.New("stale-sha")
	}

	blobSHA = r.putObjectLocked(ObjBlob, content)
	files[clean] = fileRef{OID: blobSHA, Mode: "100644"}
	treeOID := r.buildTreeFromFilesLocked(files)

	var parents []string
	if currentTip != "" {
		parents = []string{currentTip}
	}
	commitData := buildCommit(treeOID, parents, message, author, time.Now().Unix())
	commitSHA = r.putObjectLocked(ObjCommit, commitData)
	r.Refs[branchRef] = commitSHA
	r.invalidateGitCaches()
	return commitSHA, blobSHA, exists, nil
}

func (r *StoredRepo) DeleteFile(branchName, filePath, message, author string, expectedSHA *string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	branchRef := "refs/heads/" + branchName
	currentTip := r.Refs[branchRef]
	if currentTip == "" {
		return "", errors.New("branch-not-found")
	}
	clean := strings.Trim(filePath, "/")
	files := make(map[string]fileRef)
	if cObj, ok := r.Objects[currentTip]; ok {
		r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", files)
	}
	existing, exists := files[clean]
	if !exists {
		return "", errors.New("file-not-found")
	}
	if expectedSHA != nil && *expectedSHA != "" && existing.OID != *expectedSHA {
		return "", errors.New("stale-sha")
	}
	delete(files, clean)
	treeOID := r.buildTreeFromFilesLocked(files)
	commitData := buildCommit(treeOID, []string{currentTip}, message, author, time.Now().Unix())
	commitSHA := r.putObjectLocked(ObjCommit, commitData)
	r.Refs[branchRef] = commitSHA
	r.invalidateGitCaches()
	return commitSHA, nil
}

func (r *StoredRepo) CreatePull(title, body, headRefName, baseRefName string) (*StoredPull, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	headOID := r.Refs[headRefName]
	if headOID == "" {
		headOID = r.Refs["refs/heads/"+headRefName]
	}
	baseOID := r.Refs[baseRefName]
	if baseOID == "" {
		baseOID = r.Refs["refs/heads/"+baseRefName]
	}
	if headOID == "" || baseOID == "" {
		return nil, errors.New("ref-not-found")
	}

	baseFiles := make(map[string]fileRef)
	if cObj, ok := r.Objects[baseOID]; ok {
		r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", baseFiles)
	}
	headFiles := make(map[string]fileRef)
	if cObj, ok := r.Objects[headOID]; ok {
		r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", headFiles)
	}

	files := make([]PullFile, 0)
	for path, info := range headFiles {
		if bInfo, ok := baseFiles[path]; !ok {
			files = append(files, PullFile{Filename: path, Status: "added", Additions: 1, Deletions: 0, Changes: 1})
		} else if bInfo.OID != info.OID {
			files = append(files, PullFile{Filename: path, Status: "modified", Additions: 1, Deletions: 1, Changes: 2})
		}
	}
	for path := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			files = append(files, PullFile{Filename: path, Status: "removed", Additions: 0, Deletions: 1, Changes: 1})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Filename < files[j].Filename })

	r.PullSeq++
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	pr := &StoredPull{
		ID:        r.PullSeq,
		Number:    r.PullSeq,
		Title:     title,
		Body:      body,
		State:     "open",
		Head:      PullRef{Ref: strings.TrimPrefix(headRefName, "refs/heads/"), SHA: headOID},
		Base:      PullRef{Ref: strings.TrimPrefix(baseRefName, "refs/heads/"), SHA: baseOID},
		Merged:    false,
		CreatedAt: now,
		UpdatedAt: now,
		Files:     files,
	}
	r.Pulls = append(r.Pulls, pr)
	return pr, nil
}

func (r *StoredRepo) GetPull(num int) *StoredPull {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if num < 1 || num > len(r.Pulls) {
		return nil
	}
	return r.Pulls[num-1]
}

func (r *StoredRepo) ListPulls(state string) []*StoredPull {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*StoredPull, 0, len(r.Pulls))
	for _, p := range r.Pulls {
		if state == "all" || p.State == state {
			out = append(out, p)
		}
	}
	return out
}

func (r *StoredRepo) MergePull(num int, checkConflict bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if num < 1 || num > len(r.Pulls) {
		return "", errors.New("pull-not-found")
	}
	pr := r.Pulls[num-1]
	if pr.Merged || pr.State == "closed" {
		return "", errors.New("already-merged")
	}
	baseRefKey := "refs/heads/" + pr.Base.Ref
	currentBaseTip := r.Refs[baseRefKey]
	headTip := r.Refs["refs/heads/"+pr.Head.Ref]
	if headTip == "" {
		headTip = pr.Head.SHA
	}
	if currentBaseTip == "" || headTip == "" {
		return "", errors.New("ref-not-found")
	}

	if checkConflict && currentBaseTip != pr.Base.SHA {
		initBase := make(map[string]fileRef)
		if cObj, ok := r.Objects[pr.Base.SHA]; ok {
			r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", initBase)
		}
		currBase := make(map[string]fileRef)
		if cObj, ok := r.Objects[currentBaseTip]; ok {
			r.getTreeEntriesLocked(parseCommit(cObj.Data).Tree, "", currBase)
		}
		for _, hf := range pr.Files {
			if initBase[hf.Filename].OID != currBase[hf.Filename].OID {
				return "", errors.New("conflict")
			}
		}
	}

	headObj, ok := r.Objects[headTip]
	if !ok {
		return "", errors.New("ref-not-found")
	}
	headCommit := parseCommit(headObj.Data)
	msg := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.Head.Ref + "\n\n" + pr.Title
	commitData := buildCommit(headCommit.Tree, []string{currentBaseTip}, msg, "Merge Action <merge@make.fun>", time.Now().Unix())
	mergeSHA := r.putObjectLocked(ObjCommit, commitData)
	r.Refs[baseRefKey] = mergeSHA
	r.invalidateGitCaches()

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = now
	pr.MergeCommitSHA = mergeSHA
	pr.UpdatedAt = now
	return mergeSHA, nil
}

// isAncestorLocked checks whether oldOID is an ancestor of newOID in O(1) for linear pushes,
// and commit-parent-only BFS otherwise (never touches trees/blobs!).
func (r *StoredRepo) isAncestorLocked(newOID, oldOID string) bool {
	if newOID == oldOID {
		return true
	}
	obj, ok := r.Objects[newOID]
	if !ok || obj.Type != ObjCommit {
		return false
	}
	// O(1) fast path for linear push chain (99.9% of benchmark pushes!)
	if fastFirstParent(obj.Data) == oldOID {
		return true
	}
	// Commit-only BFS
	seen := make(map[string]struct{}, 16)
	queue := []string{newOID}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == oldOID {
			return true
		}
		if _, v := seen[curr]; v {
			continue
		}
		seen[curr] = struct{}{}
		cObj, ok := r.Objects[curr]
		if !ok || cObj.Type != ObjCommit {
			continue
		}
		pc := parseCommit(cObj.Data)
		for _, p := range pc.Parents {
			if p == oldOID {
				return true
			}
			if _, v := seen[p]; !v {
				queue = append(queue, p)
			}
		}
	}
	return false
}

// Global ID helpers for GraphQL
func toGlobalID(typ, id string) string {
	return base64.StdEncoding.EncodeToString([]byte(typ + ":" + id))
}

func fromGlobalID(gid string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(gid)
	if err != nil {
		return "", "", false
	}
	idx := bytes.IndexByte(raw, ':')
	if idx < 0 {
		return "", "", false
	}
	return string(raw[:idx]), string(raw[idx+1:]), true
}
