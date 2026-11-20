package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strconv"
	"strings"
)

const ZeroOID = "0000000000000000000000000000000000000000"

var zeroOIDBytes = []byte(ZeroOID)

type GitObjType uint8

const (
	ObjCommit   GitObjType = 1
	ObjTree     GitObjType = 2
	ObjBlob     GitObjType = 3
	ObjTag      GitObjType = 4
	ObjOfsDelta GitObjType = 6
	ObjRefDelta GitObjType = 7
)

func (t GitObjType) String() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	default:
		return "unknown"
	}
}

type GitObject struct {
	Type    GitObjType
	Size    int
	Data    []byte
	RawZlib []byte // cached zlib-compressed payload for zero-deflate pack emission
}

// Bytes returns the uncompressed object bytes, inflating lazily from RawZlib for blobs if needed.
func (o *GitObject) Bytes() []byte {
	if o.Data != nil || o.Size == 0 {
		return o.Data
	}
	if len(o.RawZlib) > 0 {
		raw, _, err := decompressZlib(o.RawZlib, o.Size)
		if err == nil {
			o.Data = raw
			return raw
		}
	}
	return nil
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type ParsedCommit struct {
	Tree    string
	Parents []string
	Author  string
	Message string
}

type ParsedTag struct {
	Object string
	Type   string
	Tag    string
}

// Single-threaded persistent zlib readers/writers (no sync.Pool overhead on reactor hot path)
var (
	reactorZlibBr      bytes.Reader
	reactorZlibZr      io.ReadCloser
	reactorSha1        hash.Hash = sha1.New()
	reactorScratch     [2080]byte
	reactorZlibWFast   *zlib.Writer
	reactorZlibWBest   *zlib.Writer
)

func init() {
	reactorZlibWFast, _ = zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	reactorZlibWBest, _ = zlib.NewWriterLevel(io.Discard, zlib.BestCompression)
}

func resetReactorZlibReader(src []byte) (io.ReadCloser, error) {
	reactorZlibBr.Reset(src)
	if reactorZlibZr != nil {
		if resetter, ok := reactorZlibZr.(zlib.Resetter); ok {
			if err := resetter.Reset(&reactorZlibBr, nil); err == nil {
				return reactorZlibZr, nil
			}
		}
	}
	zr, err := zlib.NewReader(&reactorZlibBr)
	if err != nil {
		return nil, err
	}
	reactorZlibZr = zr
	return zr, nil
}

func decompressZlibInto(src []byte, expectedSize int, dst []byte) ([]byte, int, error) {
	zr, err := resetReactorZlibReader(src)
	if err != nil {
		return nil, 0, err
	}
	out := dst
	if len(out) < expectedSize {
		out = make([]byte, expectedSize)
	} else {
		out = out[:expectedSize]
	}
	if expectedSize > 0 {
		if _, err := io.ReadFull(zr, out); err != nil {
			return nil, 0, err
		}
	}
	var trail [8]byte
	for {
		n, err := zr.Read(trail[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		if n > 0 {
			out = append(out, trail[:n]...)
		}
	}
	consumed := len(src) - reactorZlibBr.Len()
	return out, consumed, nil
}

func decompressZlib(src []byte, expectedSize int) ([]byte, int, error) {
	return decompressZlibInto(src, expectedSize, nil)
}

// decompressAndHashBlob streams zlib output straight into SHA-1 without allocating a blob Data buffer!
func decompressAndHashBlob(src []byte, expectedSize int) (oid string, consumed int, err error) {
	zr, err := resetReactorZlibReader(src)
	if err != nil {
		return "", 0, err
	}

	var sum [20]byte
	if expectedSize <= 2000 {
		b := reactorScratch[:0]
		b = append(b, "blob "...)
		b = strconv.AppendInt(b, int64(expectedSize), 10)
		b = append(b, 0)
		hdrLen := len(b)
		if expectedSize > 0 {
			if _, rerr := io.ReadFull(zr, reactorScratch[hdrLen:hdrLen+expectedSize]); rerr != nil {
				return "", 0, rerr
			}
		}
		var trail [8]byte
		extra := false
		for {
			n, rerr := zr.Read(trail[:])
			if n > 0 {
				if !extra {
					reactorSha1.Reset()
					reactorSha1.Write(reactorScratch[:hdrLen+expectedSize])
					extra = true
				}
				reactorSha1.Write(trail[:n])
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return "", 0, rerr
			}
		}
		consumed = len(src) - reactorZlibBr.Len()
		if extra {
			reactorSha1.Sum(sum[:0])
		} else {
			sum = sha1.Sum(reactorScratch[:hdrLen+expectedSize])
		}
	} else {
		reactorSha1.Reset()
		var hdr [32]byte
		hb := hdr[:0]
		hb = append(hb, "blob "...)
		hb = strconv.AppendInt(hb, int64(expectedSize), 10)
		hb = append(hb, 0)
		reactorSha1.Write(hb)

		for {
			n, rerr := zr.Read(reactorScratch[:])
			if n > 0 {
				reactorSha1.Write(reactorScratch[:n])
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return "", 0, rerr
			}
		}
		consumed = len(src) - reactorZlibBr.Len()
		reactorSha1.Sum(sum[:0])
	}

	var hexBuf [40]byte
	hex.Encode(hexBuf[:], sum[:])
	return string(hexBuf[:]), consumed, nil
}

func compressZlib(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 32)
	reactorZlibWFast.Reset(&buf)
	_, _ = reactorZlibWFast.Write(data)
	_ = reactorZlibWFast.Close()
	return buf.Bytes()
}

func compressZlibBest(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 32)
	reactorZlibWBest.Reset(&buf)
	_, _ = reactorZlibWBest.Write(data)
	_ = reactorZlibWBest.Close()
	return buf.Bytes()
}

func hashObjectBytes(t GitObjType, data []byte, outHex *[40]byte) {
	var sum [20]byte
	if len(data) <= 2000 {
		var stack [2048]byte
		b := stack[:0]
		b = append(b, t.String()...)
		b = append(b, ' ')
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, 0)
		b = append(b, data...)
		sum = sha1.Sum(b)
	} else {
		reactorSha1.Reset()
		var hdr [32]byte
		b := hdr[:0]
		b = append(b, t.String()...)
		b = append(b, ' ')
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, 0)
		reactorSha1.Write(b)
		reactorSha1.Write(data)
		reactorSha1.Sum(sum[:0])
	}
	hex.Encode(outHex[:], sum[:])
}

func hashObject(t GitObjType, data []byte) string {
	var hexBuf [40]byte
	hashObjectBytes(t, data, &hexBuf)
	return string(hexBuf[:])
}

func isObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func parseCommit(data []byte) ParsedCommit {
	var res ParsedCommit
	sep := bytes.Index(data, []byte("\n\n"))
	header := data
	if sep >= 0 {
		header = data[:sep]
		res.Message = string(data[sep+2:])
	}
	for len(header) > 0 {
		nl := bytes.IndexByte(header, '\n')
		var line []byte
		if nl < 0 {
			line = header
			header = nil
		} else {
			line = header[:nl]
			header = header[nl+1:]
		}
		if bytes.HasPrefix(line, []byte("tree ")) && len(line) >= 45 {
			res.Tree = string(line[5:45])
		} else if bytes.HasPrefix(line, []byte("parent ")) && len(line) >= 47 {
			res.Parents = append(res.Parents, string(line[7:47]))
		} else if bytes.HasPrefix(line, []byte("author ")) {
			res.Author = string(line[7:])
		}
	}
	return res
}

// fastFirstParentBytes returns the first parent OID slice of a commit without allocating a string or slice.
func fastFirstParentBytes(data []byte) []byte {
	if len(data) >= 94 && bytes.HasPrefix(data, []byte("tree ")) && data[45] == '\n' && bytes.HasPrefix(data[46:], []byte("parent ")) {
		return data[53:93]
	}
	sep := bytes.Index(data, []byte("\n\n"))
	header := data
	if sep >= 0 {
		header = data[:sep]
	}
	for len(header) > 0 {
		nl := bytes.IndexByte(header, '\n')
		var line []byte
		if nl < 0 {
			line = header
			header = nil
		} else {
			line = header[:nl]
			header = header[nl+1:]
		}
		if bytes.HasPrefix(line, []byte("parent ")) && len(line) >= 47 {
			return line[7:47]
		}
	}
	return nil
}

// fastFirstParent returns the first parent OID of a commit without allocating a slice.
func fastFirstParent(data []byte) string {
	b := fastFirstParentBytes(data)
	if len(b) == 40 {
		return string(b)
	}
	return ""
}

func buildCommit(tree string, parents []string, message string, author string, ts int64) []byte {
	if author == "" {
		author = "Forge <forge@make.fun>"
	}
	if !strings.Contains(author, "<") {
		author = author + " <" + author + "@make.fun>"
	}
	if ts == 0 {
		ts = 1700000000
	}
	var b bytes.Buffer
	b.Grow(256 + len(message))
	b.WriteString("tree ")
	b.WriteString(tree)
	b.WriteByte('\n')
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "author %s %d +0000\n", author, ts)
	fmt.Fprintf(&b, "committer %s %d +0000\n\n", author, ts)
	b.WriteString(message)
	return b.Bytes()
}

func parseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[pos : pos+sp])
		pos += sp + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 || pos+nul+1+20 > len(data) {
			break
		}
		name := string(data[pos : pos+nul])
		pos += nul + 1
		oid := hex.EncodeToString(data[pos : pos+20])
		pos += 20
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return entries
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ki := sorted[i].Name
		if sorted[i].Mode == "40000" {
			ki += "/"
		}
		kj := sorted[j].Name
		if sorted[j].Mode == "40000" {
			kj += "/"
		}
		return ki < kj
	})
	var b bytes.Buffer
	b.Grow(len(sorted) * 40)
	for _, e := range sorted {
		mode := strings.TrimLeft(e.Mode, "0")
		if mode == "" {
			mode = "100644"
		}
		b.WriteString(mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		b.Write(raw)
	}
	return b.Bytes()
}

func parseTag(data []byte) ParsedTag {
	var t ParsedTag
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		var line []byte
		if nl < 0 {
			line = data
			data = nil
		} else {
			line = data[:nl]
			data = data[nl+1:]
		}
		if len(line) == 0 {
			break
		}
		if bytes.HasPrefix(line, []byte("object ")) {
			t.Object = string(line[7:])
		} else if bytes.HasPrefix(line, []byte("type ")) {
			t.Type = string(line[5:])
		} else if bytes.HasPrefix(line, []byte("tag ")) {
			t.Tag = string(line[4:])
		}
	}
	return t
}

// Pkt-line helpers
func appendPktLine(dst []byte, payload []byte) []byte {
	n := len(payload) + 4
	const hexDigits = "0123456789abcdef"
	dst = append(dst,
		hexDigits[(n>>12)&0xf],
		hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf],
		hexDigits[n&0xf],
	)
	return append(dst, payload...)
}

func appendPktLineStr(dst []byte, s string) []byte {
	n := len(s) + 4
	const hexDigits = "0123456789abcdef"
	dst = append(dst,
		hexDigits[(n>>12)&0xf],
		hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf],
		hexDigits[n&0xf],
	)
	return append(dst, s...)
}

func appendPktFlush(dst []byte) []byte {
	return append(dst, '0', '0', '0', '0')
}

func appendPktDelim(dst []byte) []byte {
	return append(dst, '0', '0', '0', '1')
}

type PktKind uint8

const (
	PktData PktKind = iota
	PktFlush
	PktDelim
	PktRespEnd
)

type Pkt struct {
	Kind    PktKind
	Payload []byte
}

func decodeAllPkts(data []byte) ([]Pkt, int, error) {
	var pkts []Pkt
	pos := 0
	for pos < len(data) {
		if pos+4 > len(data) {
			return nil, pos, errors.New("truncated pkt-line header")
		}
		n, err := parseHex4(data[pos : pos+4])
		if err != nil {
			return nil, pos, err
		}
		pos += 4
		switch n {
		case 0:
			pkts = append(pkts, Pkt{Kind: PktFlush})
		case 1:
			pkts = append(pkts, Pkt{Kind: PktDelim})
		case 2:
			pkts = append(pkts, Pkt{Kind: PktRespEnd})
		case 3:
			return nil, pos, errors.New("invalid pkt-line length 3")
		default:
			plen := n - 4
			if pos+plen > len(data) {
				return nil, pos, errors.New("truncated pkt-line payload")
			}
			pkts = append(pkts, Pkt{Kind: PktData, Payload: data[pos : pos+plen]})
			pos += plen
		}
	}
	return pkts, pos, nil
}

// decodeReceivePackPkts stops at the first flush packet and returns remaining bytes (packfile).
func decodeReceivePackPkts(data []byte) ([][]byte, []byte, error) {
	var lines [][]byte
	pos := 0
	for pos < len(data) {
		if pos+4 > len(data) {
			return nil, nil, errors.New("truncated pkt-line header")
		}
		n, err := parseHex4(data[pos : pos+4])
		if err != nil {
			return nil, nil, err
		}
		pos += 4
		if n == 0 {
			return lines, data[pos:], nil
		}
		if n < 4 {
			return nil, nil, errors.New("invalid pkt-line")
		}
		plen := n - 4
		if pos+plen > len(data) {
			return nil, nil, errors.New("truncated pkt-line payload")
		}
		lines = append(lines, data[pos:pos+plen])
		pos += plen
	}
	return lines, nil, nil
}

func parseHex4(b []byte) (int, error) {
	val := 0
	for i := 0; i < 4; i++ {
		c := b[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return 0, errors.New("invalid hex digit in pkt-line")
		}
		val = (val << 4) | d
	}
	return val, nil
}

// applyDelta applies a Git binary delta onto base bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		val := 0
		shift := 0
		for {
			if pos >= len(delta) {
				return 0, errors.New("truncated delta varint")
			}
			b := delta[pos]
			pos++
			val |= int(b&0x7f) << shift
			if b&0x80 == 0 {
				return val, nil
			}
			shift += 7
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch: got %d want %d", len(base), srcSize)
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dstSize)
	for pos < len(delta) {
		cmd := delta[pos]
		pos++
		if cmd&0x80 != 0 {
			var cpOff, cpSize int
			if cmd&0x01 != 0 {
				cpOff |= int(delta[pos])
				pos++
			}
			if cmd&0x02 != 0 {
				cpOff |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x04 != 0 {
				cpOff |= int(delta[pos]) << 16
				pos++
			}
			if cmd&0x08 != 0 {
				cpOff |= int(delta[pos]) << 24
				pos++
			}
			if cmd&0x10 != 0 {
				cpSize |= int(delta[pos])
				pos++
			}
			if cmd&0x20 != 0 {
				cpSize |= int(delta[pos]) << 8
				pos++
			}
			if cmd&0x40 != 0 {
				cpSize |= int(delta[pos]) << 16
				pos++
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("delta copy out of bounds")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if cmd > 0 {
			n := int(cmd)
			if pos+n > len(delta) {
				return nil, errors.New("delta insert out of bounds")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, errors.New("invalid delta opcode 0")
		}
	}
	if len(out) != dstSize {
		return nil, errors.New("delta output size mismatch")
	}
	return out, nil
}

type parsedPackObj struct {
	oid    string
	offset int
	obj    *GitObject
}

func readPackIntoRepo(pack []byte, repo *StoredRepo) error {
	return readPackIntoInternal(pack, repo.Objects, repo)
}

// ingestPackFastForwardMain validates the Git v2 packfile header + trailing SHA-1,
// locates and verifies the plain commit object matching newOID40 in stack scratch memory,
// checks that its first parent equals oldOID40 (when oldOID40 != ZeroOID), and queues the pack
// into pendingPacks with ZERO map inserts and ZERO heap string allocations.
func ingestPackFastForwardMain(pack []byte, repo *StoredRepo, oldOID40, newOID40 []byte) (fastOK bool, parentMatched bool, err error) {
	if len(pack) < 32 {
		return false, false, errors.New("packfile too short")
	}
	if pack[0] != 'P' || pack[1] != 'A' || pack[2] != 'C' || pack[3] != 'K' {
		return false, false, errors.New("invalid pack signature")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 {
		return false, false, fmt.Errorf("unsupported pack version %d", ver)
	}
	body := pack[:len(pack)-20]
	expectedTrailer := pack[len(pack)-20:]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], expectedTrailer) {
		return false, false, errors.New("packfile checksum mismatch")
	}

	if bytes.Equal(oldOID40, zeroOIDBytes) {
		packCopy := repo.allocArenaLocked(len(pack))
		copy(packCopy, pack)
		repo.pendingPacks = append(repo.pendingPacks, packCopy)
		repo.hasPendingPacks = true
		return true, true, nil
	}

	// Scan backwards from the end of the pack body for a plain ObjCommit zlib stream matching newOID40
	var scratch [2048]byte
	var calcHex [40]byte
	for i := len(body) - 6; i >= 13; i-- {
		if body[i] != 0x78 {
			continue
		}
		b1 := body[i+1]
		if b1 != 0x9c && b1 != 0x01 && b1 != 0x5e && b1 != 0xda {
			continue
		}
		size := -1
		if i >= 14 {
			h0, h1 := body[i-2], body[i-1]
			if h0&0x80 != 0 && GitObjType((h0>>4)&7) == ObjCommit && h1&0x80 == 0 {
				s := int(h0&0x0f) | (int(h1&0x7f) << 4)
				if s >= 48 && s <= 2000 {
					size = s
				}
			}
		}
		if size < 0 && i >= 15 {
			h0, h1, h2 := body[i-3], body[i-2], body[i-1]
			if h0&0x80 != 0 && GitObjType((h0>>4)&7) == ObjCommit && h1&0x80 != 0 && h2&0x80 == 0 {
				s := int(h0&0x0f) | (int(h1&0x7f) << 4) | (int(h2&0x7f) << 11)
				if s >= 2000 && s <= 65536 {
					size = s
				}
			}
		}
		if size < 0 {
			continue
		}
		var dst []byte
		if size <= len(scratch) {
			dst = scratch[:size]
		}
		raw, _, derr := decompressZlibInto(body[i:], size, dst)
		if derr != nil || len(raw) != size {
			continue
		}
		hashObjectBytes(ObjCommit, raw, &calcHex)
		if bytes.Equal(calcHex[:], newOID40) {
			fp := fastFirstParentBytes(raw)
			if len(fp) == 40 && bytes.Equal(fp, oldOID40) {
				packCopy := repo.allocArenaLocked(len(pack))
				copy(packCopy, pack)
				repo.pendingPacks = append(repo.pendingPacks, packCopy)
				repo.hasPendingPacks = true
				return true, true, nil
			}
			// Found the new commit, but its first parent is not oldOID40 (could be multi-step or non-FF)
			return false, false, nil
		}
	}
	return false, false, nil
}

// readPackInto parses a Git v2 packfile, verifies its trailing SHA-1 checksum,
// resolves OFS_DELTA & REF_DELTA, avoids allocating uncompressed blob buffers,
// and inserts directly into dest (or returns error without mutating dest on failure).
func readPackInto(pack []byte, dest map[string]*GitObject) error {
	return readPackIntoInternal(pack, dest, nil)
}

func readPackIntoInternal(pack []byte, dest map[string]*GitObject, repo *StoredRepo) error {
	if len(pack) < 32 {
		return errors.New("packfile too short")
	}
	if pack[0] != 'P' || pack[1] != 'A' || pack[2] != 'C' || pack[3] != 'K' {
		return errors.New("invalid pack signature")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 {
		return fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))

	body := pack[:len(pack)-20]
	expectedTrailer := pack[len(pack)-20:]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], expectedTrailer) {
		return errors.New("packfile checksum mismatch")
	}

	var packCopy []byte
	if repo != nil {
		packCopy = repo.allocArenaLocked(len(body))
	} else {
		packCopy = make([]byte, len(body))
	}
	copy(packCopy, body)

	var stackEntries [8]parsedPackObj
	var entries []parsedPackObj
	if count <= len(stackEntries) {
		entries = stackEntries[:0]
	} else {
		entries = make([]parsedPackObj, 0, count)
	}

	type pendingDelta struct {
		offset  int
		kind    GitObjType
		baseOff int
		baseOID string
		delta   []byte
	}
	var pending []pendingDelta

	pos := 12
	for i := 0; i < count; i++ {
		if pos >= len(packCopy) {
			return errors.New("unexpected end of packfile")
		}
		entryOffset := pos
		c := packCopy[pos]
		pos++
		objType := GitObjType((c >> 4) & 7)
		size := int(c & 0x0f)
		shift := 4
		for c&0x80 != 0 {
			if pos >= len(packCopy) {
				return errors.New("truncated object header")
			}
			c = packCopy[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}

		var baseOff int
		var baseOID string
		if objType == ObjOfsDelta {
			if pos >= len(packCopy) {
				return errors.New("truncated ofs-delta header")
			}
			b := packCopy[pos]
			pos++
			neg := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(packCopy) {
					return errors.New("truncated ofs-delta varint")
				}
				b = packCopy[pos]
				pos++
				neg = ((neg + 1) << 7) | int(b&0x7f)
			}
			baseOff = entryOffset - neg
		} else if objType == ObjRefDelta {
			if pos+20 > len(packCopy) {
				return errors.New("truncated ref-delta base oid")
			}
			baseOID = hex.EncodeToString(packCopy[pos : pos+20])
			pos += 20
		}

		zStart := pos
		if objType == ObjBlob {
			oid, consumed, err := decompressAndHashBlob(packCopy[pos:], size)
			if err != nil {
				return fmt.Errorf("zlib error at object %d: %w", i, err)
			}
			pos += consumed
			var obj *GitObject
			if repo != nil {
				obj = repo.allocObjLocked()
			} else {
				obj = &GitObject{}
			}
			obj.Type = ObjBlob
			obj.Size = size
			obj.RawZlib = packCopy[zStart : zStart+consumed]
			entries = append(entries, parsedPackObj{oid: oid, offset: entryOffset, obj: obj})
			continue
		}

		var dst []byte
		if repo != nil && size > 0 && size <= 8192 {
			dst = repo.allocArenaLocked(size)
		}
		raw, consumed, err := decompressZlibInto(packCopy[pos:], size, dst)
		if err != nil {
			return fmt.Errorf("zlib error at object %d: %w", i, err)
		}
		pos += consumed

		if objType == ObjOfsDelta || objType == ObjRefDelta {
			pending = append(pending, pendingDelta{
				offset:  entryOffset,
				kind:    objType,
				baseOff: baseOff,
				baseOID: baseOID,
				delta:   raw,
			})
			continue
		}

		oid := hashObject(objType, raw)
		var obj *GitObject
		if repo != nil {
			obj = repo.allocObjLocked()
		} else {
			obj = &GitObject{}
		}
		obj.Type = objType
		obj.Size = len(raw)
		obj.Data = raw
		obj.RawZlib = packCopy[zStart : zStart+consumed]
		entries = append(entries, parsedPackObj{oid: oid, offset: entryOffset, obj: obj})
	}

	if len(pending) > 0 {
		byOffset := make(map[int]*GitObject, len(entries)+len(pending))
		byOID := make(map[string]*GitObject, len(entries)+len(pending))
		for _, e := range entries {
			byOffset[e.offset] = e.obj
			byOID[e.oid] = e.obj
		}
		for len(pending) > 0 {
			progressed := false
			next := pending[:0]
			for _, pd := range pending {
				var base *GitObject
				if pd.kind == ObjOfsDelta {
					base = byOffset[pd.baseOff]
				} else {
					if b, ok := byOID[pd.baseOID]; ok {
						base = b
					} else if b, ok := dest[pd.baseOID]; ok {
						base = b
					}
				}
				if base == nil {
					next = append(next, pd)
					continue
				}
				resolvedData, err := applyDelta(base.Bytes(), pd.delta)
				if err != nil {
					return err
				}
				oid := hashObject(base.Type, resolvedData)
				obj := &GitObject{
					Type: base.Type,
					Size: len(resolvedData),
					Data: resolvedData,
				}
				entries = append(entries, parsedPackObj{oid: oid, offset: pd.offset, obj: obj})
				byOffset[pd.offset] = obj
				byOID[oid] = obj
				progressed = true
			}
			if !progressed {
				return errors.New("unresolved delta base in packfile")
			}
			pending = next
		}
	}

	for _, e := range entries {
		dest[e.oid] = e.obj
	}
	return nil
}

// writePack encodes objects into a valid Git v2 packfile, reusing cached RawZlib bytes whenever available!
func writePack(objs []*GitObject) []byte {
	return writePackWithCompression(objs, false)
}

// writePackLevel9 recompresses objects at zlib level 9 (BestCompression) for the cached depth-1 clone response,
// minimizing wire payload size and client SHA-1 checksum cost across 150 concurrent clones.
func writePackLevel9(objs []*GitObject) []byte {
	return writePackWithCompression(objs, true)
}

func writePackWithCompression(objs []*GitObject, forceLevel9 bool) []byte {
	est := 32
	for _, o := range objs {
		if !forceLevel9 && len(o.RawZlib) > 0 {
			est += len(o.RawZlib) + 8
		} else {
			est += len(o.Data)/2 + 32
		}
	}
	buf := make([]byte, 12, est)
	buf[0], buf[1], buf[2], buf[3] = 'P', 'A', 'C', 'K'
	binary.BigEndian.PutUint32(buf[4:8], 2)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(objs)))

	for _, o := range objs {
		size := o.Size
		if size == 0 && len(o.Data) > 0 {
			size = len(o.Data)
		}
		first := byte((uint8(o.Type)&7)<<4) | byte(size&0x0f)
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
		var z []byte
		if forceLevel9 {
			z = compressZlibBest(o.Bytes())
			if len(o.RawZlib) == 0 || len(z) < len(o.RawZlib) {
				o.RawZlib = z
			}
		} else {
			z = o.RawZlib
			if len(z) == 0 {
				z = compressZlib(o.Bytes())
				o.RawZlib = z
			}
		}
		buf = append(buf, z...)
	}

	sum := sha1.Sum(buf)
	buf = append(buf, sum[:]...)
	return buf
}
