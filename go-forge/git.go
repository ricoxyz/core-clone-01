package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const ZeroOID = "0000000000000000000000000000000000000000"

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
	Data    []byte
	RawZlib []byte // cached zlib-compressed payload for zero-deflate pack emission
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

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return w
	},
}

type pooledZlib struct {
	br bytes.Reader
	zr io.ReadCloser
}

var zlibReaderPool = sync.Pool{}

func decompressZlib(src []byte, expectedSize int) ([]byte, int, error) {
	var pz *pooledZlib
	if pooled := zlibReaderPool.Get(); pooled != nil {
		pz = pooled.(*pooledZlib)
		pz.br.Reset(src)
		if resetter, ok := pz.zr.(zlib.Resetter); ok {
			if err := resetter.Reset(&pz.br, nil); err != nil {
				pz = nil
			}
		} else {
			pz = nil
		}
	}
	if pz == nil {
		pz = &pooledZlib{}
		pz.br.Reset(src)
		zr, err := zlib.NewReader(&pz.br)
		if err != nil {
			return nil, 0, err
		}
		pz.zr = zr
	}
	out := make([]byte, expectedSize)
	if expectedSize > 0 {
		if _, err := io.ReadFull(pz.zr, out); err != nil {
			_ = pz.zr.Close()
			return nil, 0, err
		}
	}
	var trail [8]byte
	for {
		n, err := pz.zr.Read(trail[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = pz.zr.Close()
			return nil, 0, err
		}
		if n > 0 {
			out = append(out, trail[:n]...)
		}
	}
	consumed := len(src) - pz.br.Len()
	zlibReaderPool.Put(pz)
	return out, consumed, nil
}

func compressZlib(data []byte) []byte {
	var buf bytes.Buffer
	buf.Grow(len(data)/2 + 32)
	w := zlibWriterPool.Get().(*zlib.Writer)
	w.Reset(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	zlibWriterPool.Put(w)
	return buf.Bytes()
}

func hashObject(t GitObjType, data []byte) string {
	var sum [20]byte
	if len(data) <= 2000 {
		// Stack-allocated single-pass SHA-1 (zero heap escape for all bench.push blobs/trees/commits!)
		var stack [2048]byte
		b := stack[:0]
		b = append(b, t.String()...)
		b = append(b, ' ')
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, 0)
		b = append(b, data...)
		sum = sha1.Sum(b)
	} else {
		h := sha1.New()
		var hdr [32]byte
		b := hdr[:0]
		b = append(b, t.String()...)
		b = append(b, ' ')
		b = strconv.AppendInt(b, int64(len(data)), 10)
		b = append(b, 0)
		h.Write(b)
		h.Write(data)
		h.Sum(sum[:0])
	}
	var hexBuf [40]byte
	hex.Encode(hexBuf[:], sum[:])
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

// fastFirstParent returns the first parent OID of a commit without allocating a slice.
func fastFirstParent(data []byte) string {
	// Usually line 1 is "tree <40>\n", line 2 is "parent <40>\n"
	if len(data) >= 94 && bytes.HasPrefix(data, []byte("tree ")) && data[45] == '\n' && bytes.HasPrefix(data[46:], []byte("parent ")) {
		return string(data[53:93])
	}
	p := parseCommit(data)
	if len(p.Parents) > 0 {
		return p.Parents[0]
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

// readPack parses a Git v2 packfile, verifies its trailing SHA-1 checksum,
// resolves OFS_DELTA & REF_DELTA, and preserves raw compressed zlib bytes for fast re-emission.
func readPack(pack []byte, resolveBase func(oid string) (*GitObject, bool)) (map[string]*GitObject, error) {
	if len(pack) < 32 {
		return nil, errors.New("packfile too short")
	}
	if pack[0] != 'P' || pack[1] != 'A' || pack[2] != 'C' || pack[3] != 'K' {
		return nil, errors.New("invalid pack signature")
	}
	ver := binary.BigEndian.Uint32(pack[4:8])
	if ver != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))

	// Verify trailing 20-byte SHA-1 checksum
	body := pack[:len(pack)-20]
	expectedTrailer := pack[len(pack)-20:]
	sum := sha1.Sum(body)
	if !bytes.Equal(sum[:], expectedTrailer) {
		return nil, errors.New("packfile checksum mismatch")
	}

	objs := make(map[string]*GitObject, count)
	byOffset := make(map[int]*GitObject, count)

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
		if pos >= len(body) {
			return nil, errors.New("unexpected end of packfile")
		}
		entryOffset := pos
		c := body[pos]
		pos++
		objType := GitObjType((c >> 4) & 7)
		size := int(c & 0x0f)
		shift := 4
		for c&0x80 != 0 {
			if pos >= len(body) {
				return nil, errors.New("truncated object header")
			}
			c = body[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}

		var baseOff int
		var baseOID string
		if objType == ObjOfsDelta {
			if pos >= len(body) {
				return nil, errors.New("truncated ofs-delta header")
			}
			b := body[pos]
			pos++
			neg := int(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= len(body) {
					return nil, errors.New("truncated ofs-delta varint")
				}
				b = body[pos]
				pos++
				neg = ((neg + 1) << 7) | int(b&0x7f)
			}
			baseOff = entryOffset - neg
		} else if objType == ObjRefDelta {
			if pos+20 > len(body) {
				return nil, errors.New("truncated ref-delta base oid")
			}
			baseOID = hex.EncodeToString(body[pos : pos+20])
			pos += 20
		}

		zStart := pos
		raw, consumed, err := decompressZlib(body[pos:], size)
		if err != nil {
			return nil, fmt.Errorf("zlib error at object %d: %w", i, err)
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

		// Copy raw zlib slice so pack buffer can be GC'd cleanly
		zCopy := make([]byte, consumed)
		copy(zCopy, body[zStart:zStart+consumed])

		oid := hashObject(objType, raw)
		obj := &GitObject{
			Type:    objType,
			Data:    raw,
			RawZlib: zCopy,
		}
		objs[oid] = obj
		byOffset[entryOffset] = obj
	}

	// Resolve deltas (support chained deltas)
	for len(pending) > 0 {
		progressed := false
		next := pending[:0]
		for _, pd := range pending {
			var base *GitObject
			if pd.kind == ObjOfsDelta {
				base = byOffset[pd.baseOff]
			} else {
				if b, ok := objs[pd.baseOID]; ok {
					base = b
				} else if resolveBase != nil {
					if b, ok := resolveBase(pd.baseOID); ok {
						base = b
					}
				}
			}
			if base == nil {
				next = append(next, pd)
				continue
			}
			resolvedData, err := applyDelta(base.Data, pd.delta)
			if err != nil {
				return nil, err
			}
			oid := hashObject(base.Type, resolvedData)
			obj := &GitObject{
				Type: base.Type,
				Data: resolvedData,
			}
			objs[oid] = obj
			byOffset[pd.offset] = obj
			progressed = true
		}
		if !progressed {
			return nil, errors.New("unresolved delta base in packfile")
		}
		pending = next
	}

	return objs, nil
}

// writePack encodes objects into a valid Git v2 packfile, reusing cached RawZlib bytes whenever available!
func writePack(objs []*GitObject) []byte {
	est := 32
	for _, o := range objs {
		if len(o.RawZlib) > 0 {
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
		size := len(o.Data)
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
		z := o.RawZlib
		if len(z) == 0 {
			z = compressZlib(o.Data)
			o.RawZlib = z
		}
		buf = append(buf, z...)
	}

	sum := sha1.Sum(buf)
	buf = append(buf, sum[:]...)
	return buf
}
