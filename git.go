package main

// The git side: object model, packfile writer and reader, deltas, trees, commits and pkt-lines.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func hashObject(kind string, data []byte) string {
	header := []byte(kind + " " + strconv.Itoa(len(data)) + "\x00")
	h := sha1.New()
	h.Write(header)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Repo) PutObject(kind string, data []byte) string {
	id := hashObject(kind, data)
	if _, exists := r.Objects[id]; !exists {
		r.Objects[id] = &GitObject{Type: kind, Data: data}
	}
	return id
}

func (r *Repo) ReadObject(id string) *GitObject { return r.Objects[id] }

type TreeEntry struct {
	Mode string
	Name string
	SHA  string
}

func treeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return treeSortKey(sorted[i]) < treeSortKey(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.SHA)
		buf.Write(raw)
	}
	return buf.Bytes()
}

func ParseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	at := 0
	for at < len(data) {
		sp := bytes.IndexByte(data[at:], ' ')
		if sp < 0 {
			break
		}
		sp += at
		nul := bytes.IndexByte(data[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		if nul+21 > len(data) {
			break
		}
		out = append(out, TreeEntry{
			Mode: string(data[at:sp]),
			Name: string(data[sp+1 : nul]),
			SHA:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		at = nul + 21
	}
	return out
}

func (r *Repo) FlattenTree(treeID, prefix string, out map[string]TreeEntry) {
	obj := r.ReadObject(treeID)
	if obj == nil || obj.Type != "tree" {
		return
	}
	for _, e := range ParseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			r.FlattenTree(e.SHA, path, out)
		} else {
			out[path] = TreeEntry{Mode: e.Mode, Name: path, SHA: e.SHA}
		}
	}
}

// WriteTreeFromPaths builds the nested trees for a flat path map and returns the root tree id.
func (r *Repo) WriteTreeFromPaths(files map[string]TreeEntry) string {
	type node struct {
		files map[string]TreeEntry
		dirs  map[string]*node
	}
	root := &node{files: map[string]TreeEntry{}, dirs: map[string]*node{}}
	for path, entry := range files {
		parts := strings.Split(path, "/")
		cur := root
		for i := 0; i < len(parts)-1; i++ {
			next, ok := cur.dirs[parts[i]]
			if !ok {
				next = &node{files: map[string]TreeEntry{}, dirs: map[string]*node{}}
				cur.dirs[parts[i]] = next
			}
			cur = next
		}
		cur.files[parts[len(parts)-1]] = entry
	}
	var write func(n *node) string
	write = func(n *node) string {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, e := range n.files {
			entries = append(entries, TreeEntry{Mode: e.Mode, Name: name, SHA: e.SHA})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, SHA: write(child)})
		}
		return r.PutObject("tree", BuildTree(entries))
	}
	return write(root)
}

func BuildCommit(tree string, parents []string, message, name, email, when string) []byte {
	secs := epochOf(when)
	ident := name + " <" + email + "> " + strconv.FormatInt(secs, 10) + " +0000"
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + ident + "\n")
	buf.WriteString("committer " + ident + "\n\n")
	buf.WriteString(message)
	if !strings.HasSuffix(message, "\n") {
		buf.WriteString("\n")
	}
	return buf.Bytes()
}

type CommitInfo struct {
	Tree        string
	Parents     []string
	Message     string
	AuthorName  string
	AuthorEmail string
	Time        string
}

func ParseCommit(data []byte) CommitInfo {
	text := string(data)
	idx := strings.Index(text, "\n\n")
	head := text
	body := ""
	if idx >= 0 {
		head = text[:idx]
		body = text[idx+2:]
	}
	info := CommitInfo{Message: body}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			rest := line[7:]
			if lt := strings.LastIndex(rest, " <"); lt >= 0 {
				info.AuthorName = rest[:lt]
				rest = rest[lt+2:]
				if gt := strings.Index(rest, ">"); gt >= 0 {
					info.AuthorEmail = rest[:gt]
					rest = rest[gt+2:]
					fields := strings.Fields(rest)
					if len(fields) > 0 {
						if secs, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
							info.Time = isoFromUnix(secs)
						}
					}
				}
			}
		}
	}
	return info
}

// ReachableObjects: every object reachable from the tips, commits included.
func (r *Repo) ReachableObjects(tips []string) map[string]struct{} {
	out := make(map[string]struct{}, 64)
	stack := append([]string{}, tips...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := out[id]; seen {
			continue
		}
		obj := r.ReadObject(id)
		if obj == nil {
			continue
		}
		out[id] = struct{}{}
		switch obj.Type {
		case "commit":
			info := ParseCommit(obj.Data)
			stack = append(stack, info.Tree)
			stack = append(stack, info.Parents...)
		case "tree":
			for _, e := range ParseTree(obj.Data) {
				stack = append(stack, e.SHA)
			}
		case "tag":
			if target := tagTarget(obj.Data); target != "" {
				stack = append(stack, target)
			}
		}
	}
	return out
}

func (r *Repo) ReachableTree(treeID string) map[string]struct{} {
	out := make(map[string]struct{}, 32)
	stack := []string{treeID}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := out[id]; seen {
			continue
		}
		obj := r.ReadObject(id)
		if obj == nil {
			continue
		}
		out[id] = struct{}{}
		if obj.Type == "tree" {
			for _, e := range ParseTree(obj.Data) {
				stack = append(stack, e.SHA)
			}
		}
	}
	return out
}

func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
		if line == "" {
			break
		}
	}
	return ""
}

func (r *Repo) MergeBase(a, b string) (string, bool) {
	depthOf := func(tip string) map[string]int {
		depth := make(map[string]int)
		queue := []string{tip}
		levels := []int{0}
		for len(queue) > 0 {
			id := queue[0]
			level := levels[0]
			queue = queue[1:]
			levels = levels[1:]
			if _, seen := depth[id]; seen {
				continue
			}
			obj := r.ReadObject(id)
			if obj == nil || obj.Type != "commit" {
				continue
			}
			depth[id] = level
			for _, p := range ParseCommit(obj.Data).Parents {
				queue = append(queue, p)
				levels = append(levels, level+1)
			}
		}
		return depth
	}
	left := depthOf(a)
	right := depthOf(b)
	best := ""
	bestScore := 1 << 30
	for id, d := range left {
		rd, ok := right[id]
		if !ok {
			continue
		}
		if d+rd < bestScore {
			bestScore = d + rd
			best = id
		}
	}
	return best, best != ""
}

func (r *Repo) IsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]struct{}, 32)
	stack := []string{descendant}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if id == ancestor {
			return true
		}
		obj := r.ReadObject(id)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		stack = append(stack, ParseCommit(obj.Data).Parents...)
	}
	return false
}

// ---------------------------------------------------------------- packs

var typeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func packObjectHeader(kind string, size int) []byte {
	var out []byte
	first := typeCode[kind]<<4 | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	return append(out, first)
}

func (r *Repo) WritePack(ids []string) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header[0:4], "PACK")
	header[4] = 0
	header[5] = 0
	header[6] = 0
	header[7] = 2
	n := len(ids)
	header[8] = byte(n >> 24)
	header[9] = byte(n >> 16)
	header[10] = byte(n >> 8)
	header[11] = byte(n)
	buf.Write(header)
	for _, id := range ids {
		obj := r.ReadObject(id)
		if obj == nil {
			continue
		}
		buf.Write(packObjectHeader(obj.Type, len(obj.Data)))
		zw := zlib.NewWriter(&buf)
		zw.Write(obj.Data)
		zw.Close()
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

type PackResult struct {
	Objects map[string]*GitObject
}

var errPack = errors.New("pack error")

func (r *Repo) ReadPack(buffer []byte) (*PackResult, error) {
	if len(buffer) < 32 {
		return nil, errPack
	}
	if string(buffer[0:4]) != "PACK" {
		return nil, errPack
	}
	version := int(buffer[4])<<24 | int(buffer[5])<<16 | int(buffer[6])<<8 | int(buffer[7])
	if version != 2 && version != 3 {
		return nil, errPack
	}
	count := int(buffer[8])<<24 | int(buffer[9])<<16 | int(buffer[10])<<8 | int(buffer[11])
	body := buffer[:len(buffer)-20]
	trailer := hex.EncodeToString(buffer[len(buffer)-20:])
	if sha1Hex(body) != trailer {
		return nil, errPack
	}
	objects := make(map[string]*GitObject, count)
	byOffset := make(map[int]*GitObject, count)
	at := 12
	for i := 0; i < count; i++ {
		entryOffset := at
		if at >= len(buffer) {
			return nil, errPack
		}
		b := buffer[at]
		at++
		typeID := (b >> 4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if at >= len(buffer) {
				return nil, errPack
			}
			b = buffer[at]
			at++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		baseOffset := -1
		baseID := ""
		switch typeID {
		case 6:
			if at >= len(buffer) {
				return nil, errPack
			}
			c := buffer[at]
			at++
			value := int(c & 0x7f)
			for c&0x80 != 0 {
				if at >= len(buffer) {
					return nil, errPack
				}
				c = buffer[at]
				at++
				value = ((value + 1) << 7) | int(c&0x7f)
			}
			baseOffset = entryOffset - value
		case 7:
			if at+20 > len(buffer) {
				return nil, errPack
			}
			baseID = hex.EncodeToString(buffer[at : at+20])
			at += 20
		}
		data, consumed, err := inflateAt(buffer[at:])
		if err != nil {
			return nil, err
		}
		at += consumed
		switch typeID {
		case 6:
			base, ok := byOffset[baseOffset]
			if !ok {
				return nil, errPack
			}
			resolved, err := applyDelta(base.Data, data)
			if err != nil {
				return nil, err
			}
			obj := &GitObject{Type: base.Type, Data: resolved}
			byOffset[entryOffset] = obj
			objects[hashObject(obj.Type, resolved)] = obj
		case 7:
			base, ok := objects[baseID]
			if !ok {
				base = r.ReadObject(baseID)
			}
			if base == nil {
				return nil, errPack
			}
			resolved, err := applyDelta(base.Data, data)
			if err != nil {
				return nil, err
			}
			obj := &GitObject{Type: base.Type, Data: resolved}
			byOffset[entryOffset] = obj
			objects[hashObject(obj.Type, resolved)] = obj
		default:
			kind, ok := codeType[typeID]
			if !ok {
				return nil, errPack
			}
			if len(data) != size {
				return nil, errPack
			}
			obj := &GitObject{Type: kind, Data: data}
			byOffset[entryOffset] = obj
			objects[hashObject(kind, data)] = obj
		}
	}
	return &PackResult{Objects: objects}, nil
}

func inflateAt(chunk []byte) ([]byte, int, error) {
	br := bytes.NewReader(chunk)
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	return out, len(chunk) - br.Len(), nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	at := 0
	readVarint := func() int {
		value := 0
		shift := 0
		for {
			if at >= len(delta) {
				return value
			}
			b := delta[at]
			at++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value
	}
	baseSize := readVarint()
	if baseSize != len(base) {
		return nil, errPack
	}
	resultSize := readVarint()
	out := make([]byte, resultSize)
	written := 0
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			offset := 0
			size := 0
			if op&0x01 != 0 {
				offset |= int(delta[at])
				at++
			}
			if op&0x02 != 0 {
				offset |= int(delta[at]) << 8
				at++
			}
			if op&0x04 != 0 {
				offset |= int(delta[at]) << 16
				at++
			}
			if op&0x08 != 0 {
				offset |= int(delta[at]) << 24
				at++
			}
			if op&0x10 != 0 {
				size |= int(delta[at])
				at++
			}
			if op&0x20 != 0 {
				size |= int(delta[at]) << 8
				at++
			}
			if op&0x40 != 0 {
				size |= int(delta[at]) << 16
				at++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) || written+size > len(out) {
				return nil, errPack
			}
			copy(out[written:], base[offset:offset+size])
			written += size
		} else if op != 0 {
			size := int(op)
			if at+size > len(delta) || written+size > len(out) {
				return nil, errPack
			}
			copy(out[written:], delta[at:at+size])
			at += size
			written += size
		} else {
			return nil, errPack
		}
	}
	if written != resultSize {
		return nil, errPack
	}
	return out, nil
}

// ---------------------------------------------------------------- pkt-lines

func pktLine(payload []byte) []byte {
	length := len(payload) + 4
	header := []byte(strconv.FormatInt(int64(length), 16))
	for len(header) < 4 {
		header = append([]byte{'0'}, header...)
	}
	out := make([]byte, 0, length)
	out = append(out, header...)
	return append(out, payload...)
}

func pktLineString(s string) []byte { return pktLine([]byte(s)) }

var flushPkt = []byte("0000")

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
)

type packet struct {
	kind    pktKind
	payload []byte
}

func readPktLines(buffer []byte) []packet {
	var out []packet
	at := 0
	for at+4 <= len(buffer) {
		length, err := strconv.ParseInt(string(buffer[at:at+4]), 16, 32)
		if err != nil {
			break
		}
		if length == 0 {
			out = append(out, packet{kind: pktFlush})
			at += 4
			continue
		}
		if length <= 2 {
			out = append(out, packet{kind: pktDelim})
			at += 4
			continue
		}
		if at+int(length) > len(buffer) {
			break
		}
		out = append(out, packet{kind: pktData, payload: buffer[at+4 : at+int(length)]})
		at += int(length)
	}
	return out
}

func pktLinesEnd(buffer []byte) int {
	at := 0
	for at+4 <= len(buffer) {
		length, err := strconv.ParseInt(string(buffer[at:at+4]), 16, 32)
		if err != nil {
			break
		}
		if length <= 2 {
			at += 4
			continue
		}
		at += int(length)
	}
	return at
}

func (r *Repo) ShallowWalk(tips []string, depth int) (map[string]struct{}, map[string]struct{}) {
	include := make(map[string]struct{}, 16)
	boundary := make(map[string]struct{}, 8)
	seen := make(map[string]int, 16)
	type item struct {
		id    string
		level int
	}
	queue := make([]item, 0, len(tips))
	for _, tip := range tips {
		queue = append(queue, item{tip, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if prev, ok := seen[cur.id]; ok && prev <= cur.level {
			continue
		}
		seen[cur.id] = cur.level
		obj := r.ReadObject(cur.id)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		include[cur.id] = struct{}{}
		if cur.level >= depth {
			boundary[cur.id] = struct{}{}
			continue
		}
		for _, p := range ParseCommit(obj.Data).Parents {
			queue = append(queue, item{p, cur.level + 1})
		}
	}
	return include, boundary
}

// ---------------------------------------------------------------- time helpers

// epochOf parses an ISO-8601 UTC instant into Unix seconds; zero on failure.
func epochOf(iso string) int64 {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			return 0
		}
	}
	return t.Unix()
}

func isoFromUnix(secs int64) string {
	return time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05Z")
}
