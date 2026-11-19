package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// Packfile read/write (specs/git.md §8.1). The generator emits plain entries only —
// deltas are optional in both directions — but the reader resolves OFS_DELTA and
// REF_DELTA so real-world packs (and robustness) are covered.

const zeroOID = "0000000000000000000000000000000000000000"

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// ------------------------------------------------------------ pkt-lines

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, []byte(fmt.Sprintf("%04x", n))...)
	out = append(out, payload...)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktScanner walks a pkt-line stream, yielding payload lines and control markers.
type pktScanner struct {
	data []byte
	pos  int
	err  error
}

const (
	pktData = iota
	pktFlushMarker
	pktDelimMarker
	pktEndMarker
)

func (p *pktScanner) next() (int, []byte) {
	if p.err != nil || p.pos >= len(p.data) {
		return pktEndMarker, nil
	}
	if len(p.data)-p.pos < 4 {
		p.err = errors.New("truncated pkt-line header")
		return pktEndMarker, nil
	}
	var n int
	for _, c := range p.data[p.pos : p.pos+4] {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			p.err = errors.New("invalid pkt-line header")
			return pktEndMarker, nil
		}
	}
	switch n {
	case 0:
		p.pos += 4
		return pktFlushMarker, nil
	case 1:
		p.pos += 4
		return pktDelimMarker, nil
	case 2:
		p.pos += 4
		return pktEndMarker, nil
	}
	if n < 4 || p.pos+n > len(p.data) {
		p.err = errors.New("truncated pkt-line")
		return pktEndMarker, nil
	}
	payload := p.data[p.pos+4 : p.pos+n]
	p.pos += n
	return pktData, payload
}

// ------------------------------------------------------------ pack writing

// zlibWriterPool reuses BestSpeed zlib writers across pack builds: creating
// one per object re-initialises the compressor state every time, which was a
// measurable slice of fetch/pack cost before the response-body cache.
var zlibWriterPool = sync.Pool{
	New: func() any {
		zw, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
		return zw
	},
}

func buildPack(objs []*gitObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(objs)))
	var zbuf bytes.Buffer
	zw := zlibWriterPool.Get().(*zlib.Writer)
	defer zlibWriterPool.Put(zw)
	for _, o := range objs {
		size := len(o.data)
		code := typeCode[o.typ]
		// Varint header: first byte carries type + low 4 size bits.
		b := byte(code<<4) | byte(size&0x0F)
		size >>= 4
		for size > 0 {
			buf.WriteByte(b | 0x80)
			b = byte(size & 0x7F)
			size >>= 7
		}
		buf.WriteByte(b)
		zbuf.Reset()
		zw.Reset(&zbuf)
		_, _ = zw.Write(o.data)
		_ = zw.Close()
		buf.Write(zbuf.Bytes())
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// ------------------------------------------------------------ pack reading

type packEntry struct {
	offset  int
	typ     int
	size    int64
	payload []byte // inflated bytes (object content or delta stream)
	baseOff int    // OFS_DELTA: absolute offset of base entry
	baseOID string // REF_DELTA: base object id
}

// parsePack verifies magic/version/trailer and inflates every entry.
func parsePack(data []byte) ([]*packEntry, error) {
	if len(data) < 12+20 {
		return nil, errors.New("pack too short")
	}
	if string(data[:4]) != "PACK" {
		return nil, errors.New("bad pack magic")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))
	pos := 12
	end := len(data) - 20
	var entries []*packEntry
	for i := 0; i < count; i++ {
		if pos >= end {
			return nil, errors.New("pack truncated")
		}
		entryOffset := pos
		b := data[pos]
		pos++
		typ := int(b>>4) & 0x7
		size := int64(b & 0x0F)
		shift := uint(4)
		for b&0x80 != 0 {
			b = data[pos]
			pos++
			size |= int64(b&0x7F) << shift
			shift += 7
		}
		e := &packEntry{offset: entryOffset, typ: typ, size: size}
		switch typ {
		case 6: // OFS_DELTA
			b = data[pos]
			pos++
			off := int64(b & 0x7F)
			for b&0x80 != 0 {
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7F)
			}
			e.baseOff = entryOffset - int(off)
		case 7: // REF_DELTA
			if pos+20 > end {
				return nil, errors.New("pack truncated at ref-delta base")
			}
			e.baseOID = fmt.Sprintf("%x", data[pos:pos+20])
			pos += 20
		case 1, 2, 3, 4:
		default:
			return nil, fmt.Errorf("unknown pack object type %d", typ)
		}
		br := bytes.NewReader(data[pos:end])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("zlib header: %w", err)
		}
		payload, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %w", err)
		}
		if int64(len(payload)) != size {
			return nil, fmt.Errorf("inflated size %d != declared %d", len(payload), size)
		}
		e.payload = payload
		pos = end - br.Len()
		entries = append(entries, e)
	}
	if pos != end {
		return nil, errors.New("stray bytes before pack trailer")
	}
	return entries, nil
}

// applyDelta applies a git delta stream to base bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7F) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, errors.New("delta source size mismatch")
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					off |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					size |= int64(delta[pos]) << uint(8*i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if int64(out.Len()) != tgtSize {
		return nil, errors.New("delta target size mismatch")
	}
	return out.Bytes(), nil
}

// unpackPack parses a received pack and stores every resolved object in the repo.
// Returns an error string for the report's "unpack <detail>" line, "" on success.
func (rp *repo) unpackPack(data []byte) string {
	entries, err := parsePack(data)
	if err != nil {
		return err.Error()
	}
	byOffset := map[int]*packEntry{}
	for _, e := range entries {
		byOffset[e.offset] = e
	}
	resolved := map[int]*gitObject{}
	resolving := map[int]bool{}
	var resolve func(e *packEntry) (*gitObject, error)
	resolve = func(e *packEntry) (*gitObject, error) {
		if o, ok := resolved[e.offset]; ok {
			return o, nil
		}
		if resolving[e.offset] {
			return nil, errors.New("delta cycle")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var obj *gitObject
		switch e.typ {
		case 1, 2, 3, 4:
			obj = &gitObject{typ: codeType[e.typ], data: e.payload}
		case 6:
			baseEntry, ok := byOffset[e.baseOff]
			if !ok {
				return nil, errors.New("ofs-delta base not in pack")
			}
			base, err := resolve(baseEntry)
			if err != nil {
				return nil, err
			}
			out, err := applyDelta(base.data, e.payload)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: base.typ, data: out}
		case 7:
			var base *gitObject
			// Base may be an entry of this same pack.
			for _, cand := range entries {
				if cand.typ >= 1 && cand.typ <= 4 {
					if hashObject(codeType[cand.typ], cand.payload) == e.baseOID {
						o, err := resolve(cand)
						if err != nil {
							return nil, err
						}
						base = o
						break
					}
				}
			}
			if base == nil {
				base = rp.get(e.baseOID)
			}
			if base == nil {
				return nil, errors.New("ref-delta base not found")
			}
			out, err := applyDelta(base.data, e.payload)
			if err != nil {
				return nil, err
			}
			obj = &gitObject{typ: base.typ, data: out}
		}
		resolved[e.offset] = obj
		return obj, nil
	}
	for _, e := range entries {
		obj, err := resolve(e)
		if err != nil {
			return err.Error()
		}
		rp.objects[hashObject(obj.typ, obj.data)] = obj
	}
	return ""
}

// sortedObjects materialises a deterministic entry order for pack generation.
func sortedObjects(set map[string]*gitObject) []*gitObject {
	oids := make([]string, 0, len(set))
	for oid := range set {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	out := make([]*gitObject, 0, len(oids))
	for _, oid := range oids {
		out = append(out, set[oid])
	}
	return out
}
