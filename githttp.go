package main

// git smart HTTP: upload-pack protocol v2 (ls-refs, fetch, shallow) and receive-pack v0.

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const agentLine = "agent=makefun-forge/1.0"
const zeroOID = "0000000000000000000000000000000000000000"

func refList(repo *Repo) []string {
	names := make([]string, 0, len(repo.Refs))
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func peel(repo *Repo, sha string) string {
	current := sha
	for guard := 0; guard < 16; guard++ {
		obj := repo.ReadObject(current)
		if obj == nil {
			return ""
		}
		if obj.Type == "commit" {
			return current
		}
		if obj.Type != "tag" {
			return ""
		}
		next := tagTarget(obj.Data)
		if next == "" {
			return ""
		}
		current = next
	}
	return ""
}

func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	w.Write(body)
}

func handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo, service string) {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		notFound(w)
		return
	}
	wantsV2 := service == "git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	var out []byte
	out = append(out, pktLineString("# service="+service+"\n")...)
	out = append(out, flushPkt...)
	if wantsV2 {
		out = append(out, pktLineString("version 2\n")...)
		out = append(out, pktLineString(agentLine+"\n")...)
		out = append(out, pktLineString("ls-refs=unborn\n")...)
		out = append(out, pktLineString("fetch=shallow wait-for-done\n")...)
		out = append(out, pktLineString("server-option\n")...)
		out = append(out, pktLineString("object-format=sha1\n")...)
		out = append(out, flushPkt...)
		writeBody(w, 200, "application/x-"+service+"-advertisement", out)
		return
	}
	caps := agentLine + " side-band-64k ofs-delta shallow no-progress"
	if service == "git-receive-pack" {
		caps = agentLine + " report-status side-band-64k delete-refs ofs-delta"
	}
	names := refList(repo)
	if len(names) == 0 {
		out = append(out, pktLineString(zeroOID+" capabilities^{}\x00"+caps+"\n")...)
	} else {
		for i, name := range names {
			sha := repo.Refs[name]
			if i == 0 {
				out = append(out, pktLineString(sha+" "+name+"\x00"+caps+"\n")...)
			} else {
				out = append(out, pktLineString(sha+" "+name+"\n")...)
			}
			if peeled := peel(repo, sha); peeled != "" && peeled != sha {
				out = append(out, pktLineString(peeled+" "+name+"^{}\n")...)
			}
		}
	}
	out = append(out, flushPkt...)
	writeBody(w, 200, "application/x-"+service+"-advertisement", out)
}

func handleLsRefs(w http.ResponseWriter, repo *Repo, args []string) {
	key := "ls|" + strings.Join(args, "|")
	if cached, ok := repo.BodyCache[key]; ok {
		writeBody(w, 200, "application/x-git-upload-pack-result", cached)
		return
	}
	peelWanted, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peelWanted = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSpace(a[11:]))
		}
	}
	match := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	names := refList(repo)
	head, hasHead := repo.Refs["refs/heads/"+repo.DefaultBranch]
	if len(names) == 0 && unborn && match("HEAD") {
		out = append(out, pktLineString("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n")...)
	} else if hasHead && match("HEAD") {
		line := head + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		out = append(out, pktLineString(line+"\n")...)
	}
	for _, name := range names {
		if !match(name) {
			continue
		}
		sha := repo.Refs[name]
		line := sha + " " + name
		if peelWanted {
			if peeled := peel(repo, sha); peeled != "" && peeled != sha {
				line += " peeled:" + peeled
			}
		}
		out = append(out, pktLineString(line+"\n")...)
	}
	out = append(out, flushPkt...)
	if len(repo.BodyCache) > 32 {
		repo.BodyCache = make(map[string][]byte)
	}
	repo.BodyCache[key] = out
	writeBody(w, 200, "application/x-git-upload-pack-result", out)
}

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	packets := readPktLines(body)
	var commands []string
	var args []string
	phase := 0
	for _, p := range packets {
		if p.kind != pktData {
			phase = 1
			continue
		}
		text := string(p.payload)
		if phase == 0 {
			commands = append(commands, text)
		} else {
			args = append(args, strings.TrimRight(text, "\n"))
		}
	}
	isLsRefs, isFetch := false, false
	for _, c := range commands {
		if strings.Contains(c, "command=ls-refs") {
			isLsRefs = true
		}
		if strings.Contains(c, "command=fetch") {
			isFetch = true
		}
	}
	if isLsRefs {
		handleLsRefs(w, repo, args)
		return
	}
	if isFetch {
		handleFetch(w, repo, args)
		return
	}
	writeBody(w, 200, "application/x-git-upload-pack-result", flushPkt)
}

func handleFetch(w http.ResponseWriter, repo *Repo, args []string) {
	var wants, haves, shallow []string
	depth := -1
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "deepen "):
			if v, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
				depth = v
			}
		case strings.HasPrefix(a, "shallow "):
			shallow = append(shallow, strings.TrimSpace(a[8:]))
		}
	}

	sortedWants := append([]string{}, wants...)
	sort.Strings(sortedWants)
	sortedHaves := append([]string{}, haves...)
	sort.Strings(sortedHaves)
	sortedShallow := append([]string{}, shallow...)
	sort.Strings(sortedShallow)
	cacheKey := strings.Join(sortedWants, ",") + "|" + strconv.Itoa(depth) + "|" + strings.Join(sortedHaves, ",") + "|" + strings.Join(sortedShallow, ",")
	if cached, ok := repo.BodyCache["fetch|"+cacheKey]; ok {
		writeBody(w, 200, "application/x-git-upload-pack-result", cached)
		return
	}

	var out []byte
	if len(haves) > 0 {
		out = append(out, pktLineString("acknowledgments\n")...)
		known := 0
		for _, id := range haves {
			if repo.ReadObject(id) != nil {
				out = append(out, pktLineString("ACK "+id+"\n")...)
				known++
			}
		}
		if known == 0 {
			out = append(out, pktLineString("NAK\n")...)
		} else {
			out = append(out, pktLineString("ready\n")...)
		}
		out = append(out, flushPkt...)
	}

	var wantCommits []string
	for _, id := range wants {
		if repo.ReadObject(id) != nil {
			wantCommits = append(wantCommits, id)
		}
	}
	var include map[string]struct{}
	boundary := map[string]struct{}{}
	if depth >= 0 {
		d := depth
		if d < 1 {
			d = 1
		}
		include, boundary = repo.ShallowWalk(wantCommits, d)
		out = append(out, pktLineString("shallow-info\n")...)
		boundaryList := make([]string, 0, len(boundary))
		for id := range boundary {
			boundaryList = append(boundaryList, id)
		}
		sort.Strings(boundaryList)
		for _, id := range boundaryList {
			out = append(out, pktLineString("shallow "+id+"\n")...)
		}
		for _, id := range shallow {
			if _, still := boundary[id]; !still {
				out = append(out, pktLineString("unshallow "+id+"\n")...)
			}
		}
		out = append(out, flushPkt...)
	} else if len(shallow) > 0 {
		out = append(out, pktLineString("shallow-info\n")...)
		for _, id := range shallow {
			out = append(out, pktLineString("unshallow "+id+"\n")...)
		}
		out = append(out, flushPkt...)
	}

	haveObjects := make(map[string]struct{}, 64)
	shallowClient := len(shallow) > 0
	for _, id := range haves {
		obj := repo.ReadObject(id)
		if obj == nil {
			continue
		}
		if shallowClient {
			haveObjects[id] = struct{}{}
			if obj.Type == "commit" {
				for k := range repo.ReachableTree(ParseCommit(obj.Data).Tree) {
					haveObjects[k] = struct{}{}
				}
			}
		} else {
			for k := range repo.ReachableObjects([]string{id}) {
				haveObjects[k] = struct{}{}
			}
		}
	}

	tips := wantCommits
	if include != nil {
		tips = make([]string, 0, len(include))
		for id := range include {
			tips = append(tips, id)
		}
	}
	send := make([]string, 0, 128)
	for id := range repo.ReachableObjects(tips) {
		if _, has := haveObjects[id]; has {
			continue
		}
		if include != nil {
			if obj := repo.ReadObject(id); obj != nil && obj.Type == "commit" {
				if _, in := include[id]; !in {
					continue
				}
			}
		}
		send = append(send, id)
	}
	sort.Strings(send)
	pack := repo.WritePack(send)

	out = append(out, pktLineString("packfile\n")...)
	for at := 0; at < len(pack); at += 65000 {
		end := at + 65000
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 1+end-at)
		chunk[0] = 1
		copy(chunk[1:], pack[at:end])
		out = append(out, pktLine(chunk)...)
	}
	out = append(out, flushPkt...)
	if len(repo.BodyCache) > 32 {
		repo.BodyCache = make(map[string][]byte)
	}
	repo.BodyCache["fetch|"+cacheKey] = out
	writeBody(w, 200, "application/x-git-upload-pack-result", out)
}

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo, body []byte) {
	packets := readPktLines(body)
	var commands []pushCommand
	caps := map[string]bool{}
	first := true
	for _, p := range packets {
		if p.kind != pktData {
			break
		}
		text := strings.TrimRight(string(p.payload), "\n")
		if first {
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				for _, c := range strings.Fields(text[nul+1:]) {
					caps[c] = true
				}
				text = text[:nul]
			}
			first = false
		}
		parts := strings.SplitN(text, " ", 3)
		if len(parts) < 3 {
			continue
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}

	packStart := pktLinesEnd(body)
	unpackOK := true
	unpackDetail := ""
	if packStart < len(body) {
		result, err := repo.ReadPack(body[packStart:])
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for id, obj := range result.Objects {
				repo.Objects[id] = obj
			}
		}
	}

	var report []byte
	if unpackOK {
		report = append(report, pktLineString("unpack ok\n")...)
	} else {
		report = append(report, pktLineString("unpack error "+unpackDetail+"\n")...)
	}
	for _, cmd := range commands {
		current, hasCurrent := repo.Refs[cmd.ref]
		isDelete := cmd.newOID == zeroOID
		isCreate := cmd.oldOID == zeroOID
		if !unpackOK {
			report = append(report, pktLineString("ng "+cmd.ref+" unpacker error\n")...)
			continue
		}
		if isCreate {
			if hasCurrent {
				report = append(report, pktLineString("ng "+cmd.ref+" already exists\n")...)
				continue
			}
			if isDelete {
				report = append(report, pktLineString("ng "+cmd.ref+" cannot delete a missing ref\n")...)
				continue
			}
			if repo.ReadObject(cmd.newOID) == nil {
				report = append(report, pktLineString("ng "+cmd.ref+" missing object\n")...)
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			repo.clearCaches()
			report = append(report, pktLineString("ok "+cmd.ref+"\n")...)
			continue
		}
		if !hasCurrent || current != cmd.oldOID {
			report = append(report, pktLineString("ng "+cmd.ref+" fetch first\n")...)
			continue
		}
		if isDelete {
			delete(repo.Refs, cmd.ref)
			repo.clearCaches()
			report = append(report, pktLineString("ok "+cmd.ref+"\n")...)
			continue
		}
		if repo.ReadObject(cmd.newOID) == nil {
			report = append(report, pktLineString("ng "+cmd.ref+" missing object\n")...)
			continue
		}
		if !repo.IsAncestor(cmd.oldOID, cmd.newOID) {
			report = append(report, pktLineString("ng "+cmd.ref+" non-fast-forward\n")...)
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		repo.clearCaches()
		report = append(report, pktLineString("ok "+cmd.ref+"\n")...)
	}
	report = append(report, flushPkt...)

	if caps["side-band-64k"] {
		var framed []byte
		for at := 0; at < len(report); at += 65000 {
			end := at + 65000
			if end > len(report) {
				end = len(report)
			}
			chunk := make([]byte, 1+end-at)
			chunk[0] = 1
			copy(chunk[1:], report[at:end])
			framed = append(framed, pktLine(chunk)...)
		}
		framed = append(framed, flushPkt...)
		writeBody(w, 200, "application/x-git-receive-pack-result", framed)
		return
	}
	writeBody(w, 200, "application/x-git-receive-pack-result", report)
}

var _ = bytes.Equal
