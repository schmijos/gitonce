package main

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// keepUploads, when true, skips deleting zip files after serving them.
// Set KEEP_UPLOADS=true to enable.
var keepUploads = os.Getenv("KEEP_UPLOADS") == "true"

// ---------------------------------------------------------------------------
// In-memory repo registry
// ---------------------------------------------------------------------------

type memRepo struct {
	objects  map[string][]byte // sha1 hex -> raw git object (header + content)
	head     string            // sha1 of HEAD commit
	zipPath  string            // path to the source zip on disk
	mu       sync.Mutex
	consumed bool
}

var repoCache sync.Map // name -> *memRepo

func getOrLoadRepo(name string) (*memRepo, error) {
	if v, ok := repoCache.Load(name); ok {
		return v.(*memRepo), nil
	}
	zipPath := filepath.Join(uploadsDir, name+".zip")
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return nil, err
	}
	repo, err := buildRepoFromZip(data)
	if err != nil {
		return nil, err
	}
	repo.zipPath = zipPath
	actual, _ := repoCache.LoadOrStore(name, repo)
	return actual.(*memRepo), nil
}

// ---------------------------------------------------------------------------
// Git object creation
// ---------------------------------------------------------------------------

func gitObject(typ string, content []byte) (sha string, raw []byte) {
	header := fmt.Sprintf("%s %d\x00", typ, len(content))
	raw = make([]byte, len(header)+len(content))
	copy(raw, header)
	copy(raw[len(header):], content)
	h := sha1.Sum(raw)
	return hex.EncodeToString(h[:]), raw
}

type treeEntry struct {
	mode string
	name string
	sha  string
}

func treeObject(entries []treeEntry) (string, []byte) {
	// Git sorts entries by name, treating directory names as if they end in "/"
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].name, entries[j].name
		if entries[i].mode == "40000" {
			ni += "/"
		}
		if entries[j].mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %s\x00", e.mode, e.name)
		b, _ := hex.DecodeString(e.sha)
		buf.Write(b)
	}
	return gitObject("tree", buf.Bytes())
}

func commitObject(treeSHA string) (string, []byte) {
	ts := fmt.Sprintf("%d +0000", time.Now().Unix())
	author := "Upload <upload@localhost> " + ts
	content := fmt.Sprintf("tree %s\nauthor %s\ncommitter %s\n\nInitial commit\n",
		treeSHA, author, author)
	return gitObject("commit", []byte(content))
}

// ---------------------------------------------------------------------------
// Build repo from zip
// ---------------------------------------------------------------------------

func buildRepoFromZip(data []byte) (*memRepo, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}

	objects := make(map[string][]byte)
	files := make(map[string]string) // zip path -> blob sha

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		sha, raw := gitObject("blob", content)
		objects[sha] = raw
		files[f.Name] = sha
	}

	rootSHA, err := buildTree(files, "", objects)
	if err != nil {
		return nil, err
	}
	commitSHA, commitRaw := commitObject(rootSHA)
	objects[commitSHA] = commitRaw

	if debug {
		log.Printf("DEBUG git settings: author=%q tree=%s head=%s objects=%d",
			"Upload <upload@localhost>", rootSHA, commitSHA, len(objects))
	}

	return &memRepo{objects: objects, head: commitSHA}, nil
}

// buildTree recursively builds git tree objects for all entries under prefix.
func buildTree(files map[string]string, prefix string, objects map[string][]byte) (string, error) {
	seen := make(map[string]bool)
	var entries []treeEntry

	for path, blobSHA := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rel := path[len(prefix):]
		if rel == "" {
			continue
		}
		if before, _, ok := strings.Cut(rel, "/"); !ok {
			entries = append(entries, treeEntry{mode: "100644", name: rel, sha: blobSHA})
		} else {
			dir := before
			if seen[dir] {
				continue
			}
			seen[dir] = true
			subSHA, err := buildTree(files, prefix+dir+"/", objects)
			if err != nil {
				return "", err
			}
			entries = append(entries, treeEntry{mode: "40000", name: dir, sha: subSHA})
		}
	}

	sha, raw := treeObject(entries)
	objects[sha] = raw
	return sha, nil
}

// ---------------------------------------------------------------------------
// Pack file builder
// ---------------------------------------------------------------------------

func buildPack(objects map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	binary.Write(&buf, binary.BigEndian, uint32(2))
	binary.Write(&buf, binary.BigEndian, uint32(len(objects)))

	for _, raw := range objects {
		before, after, ok := bytes.Cut(raw, []byte{0})
		if !ok {
			return nil, fmt.Errorf("malformed object")
		}
		typName, _, _ := strings.Cut(string(before), " ")
		var typ int
		switch typName {
		case "commit":
			typ = 1
		case "tree":
			typ = 2
		case "blob":
			typ = 3
		default:
			return nil, fmt.Errorf("unknown object type: %s", typName)
		}
		content := after

		// Variable-length type+size header
		size := len(content)
		b := byte((typ << 4) | (size & 0x0f))
		for size >>= 4; size > 0; size >>= 7 {
			buf.WriteByte(b | 0x80)
			b = byte(size & 0x7f)
		}
		buf.WriteByte(b)

		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(content) //nolint:errcheck // bytes.Buffer write never fails
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("zlib close: %w", err)
		}
		buf.Write(zb.Bytes())
	}

	h := sha1.Sum(buf.Bytes())
	buf.Write(h[:])
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Pkt-line helpers
// ---------------------------------------------------------------------------

func pktLine(data []byte) []byte {
	out := make([]byte, 4+len(data))
	copy(out, fmt.Sprintf("%04x", len(data)+4))
	copy(out[4:], data)
	return out
}

var pktFlush = []byte("0000")

func readPktLine(r io.Reader) (data []byte, flush bool, err error) {
	var lbuf [4]byte
	if _, err = io.ReadFull(r, lbuf[:]); err != nil {
		return
	}
	var length int
	if _, err = fmt.Sscanf(string(lbuf[:]), "%04x", &length); err != nil {
		return nil, false, fmt.Errorf("invalid pkt-line header: %w", err)
	}
	if length == 0 {
		return nil, true, nil
	}
	if length < 4 {
		return nil, false, fmt.Errorf("invalid pkt-line length %d", length)
	}
	data = make([]byte, length-4)
	_, err = io.ReadFull(r, data)
	return
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleGit(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/gitonce/")
	before, after, ok := strings.Cut(path, ".git")
	if !ok {
		http.NotFound(w, r)
		return
	}
	name, suffix := before, after

	if strings.ContainsAny(name, "/\\") {
		http.NotFound(w, r)
		return
	}

	repo, err := getOrLoadRepo(name)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	repo.mu.Lock()
	consumed := repo.consumed
	repo.mu.Unlock()
	if consumed {
		http.Error(w, "gone", http.StatusGone)
		return
	}

	switch {
	case suffix == "/info/refs" && r.URL.Query().Get("service") == "git-upload-pack":
		serveInfoRefs(w, repo)
	case suffix == "/git-upload-pack":
		serveUploadPack(w, r, repo)
	default:
		http.NotFound(w, r)
	}
}

func serveInfoRefs(w http.ResponseWriter, repo *memRepo) {
	// Build the full response body before writing so we can set Content-Length.
	// Without Content-Length the response uses chunked transfer encoding, which
	// some Kubernetes ingress controllers do not forward correctly, causing the
	// client to receive an empty capability advertisement and omit side-band-64k
	// from the subsequent upload-pack request.
	var body bytes.Buffer
	body.Write(pktLine([]byte("# service=git-upload-pack\n")))
	body.Write(pktFlush)
	body.Write(pktLine(fmt.Appendf(nil, "%s HEAD\x00side-band-64k ofs-delta allow-tip-sha1-in-want allow-reachable-sha1-in-want shallow agent=git/2.0\n", repo.head)))
	body.Write(pktLine(fmt.Appendf(nil, "%s refs/heads/main\n", repo.head)))
	body.Write(pktFlush)

	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", body.Len()))
	w.Write(body.Bytes())
}

func serveUploadPack(w http.ResponseWriter, r *http.Request, repo *memRepo) {
	repo.mu.Lock()
	if repo.consumed {
		repo.mu.Unlock()
		http.Error(w, "gone", http.StatusGone)
		return
	}
	repo.consumed = true
	repo.mu.Unlock()

	sideband, deepen := scanUploadPackRequest(r.Body)

	pack, err := buildPack(repo.objects)
	if err != nil {
		http.Error(w, "pack error", http.StatusInternalServerError)
		return
	}

	// Build the full response body before writing so we can set Content-Length.
	// Without Content-Length the response uses chunked transfer encoding, which
	// some Kubernetes ingress controllers do not forward correctly for git smart
	// HTTP, causing the client to see an empty pack.
	var body bytes.Buffer
	if deepen {
		// When the client requests a shallow clone (deepen N), the git protocol
		// requires the server to send shallow boundary lines + flush before NAK.
		// Without this, go-git's shallow-update parser reads ahead past the NAK
		// pkt-line while scanning for shallow entries, consuming it — so the
		// server-response parser never sees NAK and tries to parse the sideband
		// pack header as a NAK line, resulting in "empty packfile".
		body.Write(pktLine(fmt.Appendf(nil, "shallow %s\n", repo.head)))
		body.Write(pktFlush)
	}
	body.Write(pktLine([]byte("NAK\n")))
	if sideband {
		writeSideband(&body, pack)
	} else {
		body.Write(pack)
	}

	log.Printf("upload-pack: sideband=%v pack=%d bytes objects=%d response=%d bytes",
		sideband, len(pack), len(repo.objects), body.Len())

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", body.Len()))
	w.Write(body.Bytes())

	if !keepUploads {
		if err := os.Remove(repo.zipPath); err != nil && !os.IsNotExist(err) {
			log.Printf("failed to delete zip %s: %v", repo.zipPath, err)
		}
	}
}

// scanUploadPackRequest drains the client's want/done lines and reports
// whether side-band-64k was requested and whether a deepen (shallow clone)
// was requested.
func scanUploadPackRequest(r io.ReadCloser) (sideband, deepen bool) {
	defer r.Close()
	for {
		data, flush, err := readPktLine(r)
		if err != nil {
			return
		}
		if flush {
			for {
				data2, flush2, err2 := readPktLine(r)
				if err2 != nil || flush2 || bytes.HasPrefix(data2, []byte("done")) {
					return
				}
			}
		}
		if !sideband && bytes.HasPrefix(data, []byte("want ")) {
			sideband = bytes.Contains(data, []byte("side-band-64k"))
		}
		if !deepen && bytes.HasPrefix(data, []byte("deepen ")) {
			deepen = true
		}
	}
}

// writeSideband streams pack data wrapped in side-band-64k channel-1 packets.
func writeSideband(w io.Writer, pack []byte) {
	const maxChunk = 65515 // 65520 - 4 (pkt header) - 1 (channel byte)
	chunk := make([]byte, 1+maxChunk)
	chunk[0] = 1 // data channel
	for len(pack) > 0 {
		n := copy(chunk[1:], pack)
		w.Write(pktLine(chunk[:1+n]))
		pack = pack[n:]
	}
	w.Write(pktFlush)
}
