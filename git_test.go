package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// treeEntryNames parses the entry names out of a raw tree object in order.
func treeEntryNames(raw []byte) []string {
	null := bytes.IndexByte(raw, 0)
	content := raw[null+1:] // skip "tree N\x00"
	var names []string
	for len(content) > 0 {
		null := bytes.IndexByte(content, 0)
		// content[:null] is "mode name"
		parts := strings.SplitN(string(content[:null]), " ", 2)
		names = append(names, parts[1])
		content = content[null+1+20:] // skip NUL + 20-byte SHA
	}
	return names
}

// packObjectCount reads the object count from a packfile header.
func packObjectCount(pack []byte) uint32 {
	return binary.BigEndian.Uint32(pack[8:12])
}

// packSHA1Valid checks that the trailing 20 bytes match SHA1 of the rest.
func packSHA1Valid(pack []byte) bool {
	body := pack[:len(pack)-20]
	want := sha1.Sum(body)
	return bytes.Equal(pack[len(pack)-20:], want[:])
}

// readAllPktLines reads all non-flush pkt-lines from r.
func readAllPktLines(r io.Reader) [][]byte {
	var out [][]byte
	for {
		data, flush, err := readPktLine(r)
		if err != nil || flush {
			return out
		}
		out = append(out, data)
	}
}

// makeTestZip builds an in-memory zip with the given name→content entries.
func makeTestZip(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

// uploadPackRequest builds a minimal git-upload-pack request body.
func uploadPackRequest(sha string, caps ...string) io.ReadCloser {
	capStr := strings.Join(caps, " ")
	line := "want " + sha
	if capStr != "" {
		line += " " + capStr
	}
	line += "\n"
	var buf bytes.Buffer
	buf.Write(pktLine([]byte(line)))
	buf.Write(pktFlush)
	buf.Write(pktLine([]byte("done\n")))
	return io.NopCloser(&buf)
}

// ---------------------------------------------------------------------------
// gitObject
// ---------------------------------------------------------------------------

func TestGitObject_Format(t *testing.T) {
	_, raw := gitObject("blob", []byte("hello"))
	if !bytes.HasPrefix(raw, []byte("blob 5\x00hello")) {
		t.Fatalf("unexpected raw: %q", raw)
	}
}

func TestGitObject_SHA(t *testing.T) {
	sha, raw := gitObject("blob", []byte("hello"))
	h := sha1.Sum(raw)
	if want := hex.EncodeToString(h[:]); sha != want {
		t.Fatalf("got %s, want %s", sha, want)
	}
}

func TestGitObject_EmptyContent(t *testing.T) {
	sha, raw := gitObject("blob", nil)
	if !bytes.HasPrefix(raw, []byte("blob 0\x00")) {
		t.Fatalf("unexpected raw: %q", raw)
	}
	h := sha1.Sum(raw)
	if hex.EncodeToString(h[:]) != sha {
		t.Fatal("SHA mismatch for empty blob")
	}
}

// ---------------------------------------------------------------------------
// treeObject
// ---------------------------------------------------------------------------

func TestTreeObject_Sort_DirVsFile(t *testing.T) {
	// "ab" as a dir sorts as "ab/" which is before "abc" (since '/' < 'c')
	sha0 := strings.Repeat("0", 40)
	_, raw := treeObject([]treeEntry{
		{mode: "100644", name: "abc", sha: sha0},
		{mode: "40000", name: "ab", sha: sha0},
	})
	names := treeEntryNames(raw)
	if len(names) != 2 || names[0] != "ab" || names[1] != "abc" {
		t.Fatalf("unexpected order: %v", names)
	}
}

func TestTreeObject_Sort_Files(t *testing.T) {
	sha0 := strings.Repeat("0", 40)
	_, raw := treeObject([]treeEntry{
		{mode: "100644", name: "z.txt", sha: sha0},
		{mode: "100644", name: "a.txt", sha: sha0},
		{mode: "100644", name: "m.txt", sha: sha0},
	})
	names := treeEntryNames(raw)
	if names[0] != "a.txt" || names[1] != "m.txt" || names[2] != "z.txt" {
		t.Fatalf("unexpected order: %v", names)
	}
}

func TestTreeObject_Sort_FileBeforeDir(t *testing.T) {
	// When entries start with [dir, file], sort calls less(i=file, j=dir),
	// exercising the nj += "/" branch.
	sha0 := strings.Repeat("0", 40)
	_, raw := treeObject([]treeEntry{
		{mode: "40000", name: "src", sha: sha0},
		{mode: "100644", name: "a.go", sha: sha0},
	})
	names := treeEntryNames(raw)
	if len(names) != 2 || names[0] != "a.go" || names[1] != "src" {
		t.Fatalf("unexpected order: %v", names)
	}
}

func TestTreeObject_BinaryFormat(t *testing.T) {
	blobSHA := strings.Repeat("ab", 20) // 40 hex chars
	_, raw := treeObject([]treeEntry{
		{mode: "100644", name: "file.txt", sha: blobSHA},
	})
	// Skip "tree N\x00", then check entry bytes
	null := bytes.IndexByte(raw, 0)
	content := raw[null+1:]

	want := "100644 file.txt\x00"
	if !bytes.HasPrefix(content, []byte(want)) {
		t.Fatalf("entry header %q not found in %q", want, content)
	}
	// The 20 bytes after the NUL should be the decoded SHA
	entryNUL := bytes.IndexByte(content, 0)
	gotSHA := hex.EncodeToString(content[entryNUL+1 : entryNUL+21])
	if gotSHA != blobSHA {
		t.Fatalf("SHA in tree: got %s, want %s", gotSHA, blobSHA)
	}
}

// ---------------------------------------------------------------------------
// commitObject
// ---------------------------------------------------------------------------

func TestCommitObject_Format(t *testing.T) {
	treeSHA := strings.Repeat("a", 40)
	_, raw := commitObject(treeSHA)

	null := bytes.IndexByte(raw, 0)
	content := string(raw[null+1:])

	if !strings.HasPrefix(content, "tree "+treeSHA+"\n") {
		t.Fatalf("missing tree line in: %q", content)
	}
	if !strings.Contains(content, "author ") {
		t.Fatal("missing author line")
	}
	if !strings.Contains(content, "committer ") {
		t.Fatal("missing committer line")
	}
	if !strings.HasSuffix(content, "\n\nInitial commit\n") {
		t.Fatalf("unexpected commit message ending: %q", content)
	}
}

func TestCommitObject_SHA(t *testing.T) {
	sha, raw := commitObject(strings.Repeat("a", 40))
	h := sha1.Sum(raw)
	if hex.EncodeToString(h[:]) != sha {
		t.Fatal("SHA mismatch for commit object")
	}
}

// ---------------------------------------------------------------------------
// buildTree
// ---------------------------------------------------------------------------

func TestBuildTree_Flat(t *testing.T) {
	sha1hex, _ := gitObject("blob", []byte("a"))
	sha2hex, _ := gitObject("blob", []byte("b"))
	files := map[string]string{"a.txt": sha1hex, "b.txt": sha2hex}
	objects := map[string][]byte{}

	rootSHA, err := buildTree(files, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	names := treeEntryNames(objects[rootSHA])
	if len(names) != 2 {
		t.Fatalf("expected 2 entries, got %v", names)
	}
}

func TestBuildTree_Nested(t *testing.T) {
	blobSHA, blobRaw := gitObject("blob", []byte("hello"))
	files := map[string]string{"src/main.go": blobSHA}
	objects := map[string][]byte{blobSHA: blobRaw}

	rootSHA, err := buildTree(files, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	// Root should have one dir entry "src"
	rootNames := treeEntryNames(objects[rootSHA])
	if len(rootNames) != 1 || rootNames[0] != "src" {
		t.Fatalf("expected [src], got %v", rootNames)
	}
	// Get the src tree SHA from the binary entry in the root tree object
	null := bytes.IndexByte(objects[rootSHA], 0)
	content := objects[rootSHA][null+1:]
	entryNUL := bytes.IndexByte(content, 0)
	srcTreeSHA := hex.EncodeToString(content[entryNUL+1 : entryNUL+21])
	srcNames := treeEntryNames(objects[srcTreeSHA])
	if len(srcNames) != 1 || srcNames[0] != "main.go" {
		t.Fatalf("expected [main.go] in src tree, got %v", srcNames)
	}
}

func TestBuildTree_Empty(t *testing.T) {
	objects := map[string][]byte{}
	sha, err := buildTree(map[string]string{}, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("expected non-empty SHA for empty tree")
	}
	names := treeEntryNames(objects[sha])
	if len(names) != 0 {
		t.Fatalf("expected no entries, got %v", names)
	}
}

// ---------------------------------------------------------------------------
// buildPack
// ---------------------------------------------------------------------------

func TestBuildPack_Header(t *testing.T) {
	sha, raw := gitObject("blob", []byte("test"))
	pack, err := buildPack(map[string][]byte{sha: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pack, []byte("PACK")) {
		t.Fatal("missing PACK magic")
	}
	if binary.BigEndian.Uint32(pack[4:8]) != 2 {
		t.Fatal("expected version 2")
	}
	if packObjectCount(pack) != 1 {
		t.Fatalf("expected 1 object, got %d", packObjectCount(pack))
	}
}

func TestBuildPack_TrailingSHA(t *testing.T) {
	sha, raw := gitObject("blob", []byte("test"))
	pack, err := buildPack(map[string][]byte{sha: raw})
	if err != nil {
		t.Fatal(err)
	}
	if !packSHA1Valid(pack) {
		t.Fatal("trailing SHA1 invalid")
	}
}

func TestBuildPack_MultipleObjects(t *testing.T) {
	objects := map[string][]byte{}
	for _, content := range []string{"a", "b", "c"} {
		sha, raw := gitObject("blob", []byte(content))
		objects[sha] = raw
	}
	pack, err := buildPack(objects)
	if err != nil {
		t.Fatal(err)
	}
	if packObjectCount(pack) != 3 {
		t.Fatalf("expected 3 objects, got %d", packObjectCount(pack))
	}
	if !packSHA1Valid(pack) {
		t.Fatal("trailing SHA1 invalid")
	}
}

func TestBuildPack_UnknownType(t *testing.T) {
	bad := map[string][]byte{"abc": []byte("tag 4\x00test")}
	_, err := buildPack(bad)
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestBuildPack_MalformedObject(t *testing.T) {
	bad := map[string][]byte{"abc": []byte("no-null-byte")}
	_, err := buildPack(bad)
	if err == nil {
		t.Fatal("expected error for malformed object")
	}
}

// ---------------------------------------------------------------------------
// pktLine / readPktLine
// ---------------------------------------------------------------------------

func TestPktLine_LengthEncoding(t *testing.T) {
	data := []byte("hello\n")
	out := pktLine(data)
	// Length = 4 (header) + 6 (data) = 10 = "000a"
	if string(out[:4]) != "000a" {
		t.Fatalf("got %q, want %q", out[:4], "000a")
	}
	if !bytes.Equal(out[4:], data) {
		t.Fatalf("data mismatch: %q", out[4:])
	}
}

func TestPktLine_Empty(t *testing.T) {
	out := pktLine([]byte{})
	if string(out) != "0004" {
		t.Fatalf("expected \"0004\", got %q", out)
	}
}

func TestReadPktLine_RoundTrip(t *testing.T) {
	want := []byte("# service=git-upload-pack\n")
	r := bytes.NewReader(pktLine(want))
	got, flush, err := readPktLine(r)
	if err != nil || flush {
		t.Fatalf("unexpected err=%v flush=%v", err, flush)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadPktLine_Flush(t *testing.T) {
	r := bytes.NewReader(pktFlush)
	_, flush, err := readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if !flush {
		t.Fatal("expected flush=true")
	}
}

func TestReadPktLine_BinaryData(t *testing.T) {
	want := []byte{0x01, 0x02, 0x03, 0x00, 0xff}
	r := bytes.NewReader(pktLine(want))
	got, _, err := readPktLine(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// scanUploadPackRequest
// ---------------------------------------------------------------------------

func TestScanUploadPackRequest_Sideband(t *testing.T) {
	sha := strings.Repeat("a", 40)
	if !scanUploadPackRequest(uploadPackRequest(sha, "side-band-64k", "ofs-delta")) {
		t.Fatal("expected sideband=true")
	}
}

func TestScanUploadPackRequest_NoSideband(t *testing.T) {
	sha := strings.Repeat("a", 40)
	if scanUploadPackRequest(uploadPackRequest(sha)) {
		t.Fatal("expected sideband=false")
	}
}

func TestScanUploadPackRequest_EmptyBody(t *testing.T) {
	// Should not panic or block on an empty body
	result := scanUploadPackRequest(io.NopCloser(bytes.NewReader(nil)))
	if result {
		t.Fatal("expected sideband=false for empty body")
	}
}

// ---------------------------------------------------------------------------
// writeSideband
// ---------------------------------------------------------------------------

func TestWriteSideband_ChannelByte(t *testing.T) {
	var buf bytes.Buffer
	writeSideband(&buf, []byte("hello"))

	data, flush, err := readPktLine(&buf)
	if err != nil || flush {
		t.Fatalf("err=%v flush=%v", err, flush)
	}
	if data[0] != 1 {
		t.Fatalf("expected channel byte 1, got %d", data[0])
	}
	if string(data[1:]) != "hello" {
		t.Fatalf("unexpected payload: %q", data[1:])
	}
	// Next should be flush
	_, flush, _ = readPktLine(&buf)
	if !flush {
		t.Fatal("expected trailing flush packet")
	}
}

func TestWriteSideband_Chunking(t *testing.T) {
	const maxChunk = 65515
	// Data slightly larger than one chunk
	pack := bytes.Repeat([]byte{0xAB}, maxChunk+100)

	var buf bytes.Buffer
	writeSideband(&buf, pack)

	var got []byte
	for {
		data, flush, err := readPktLine(&buf)
		if err != nil || flush {
			break
		}
		if data[0] != 1 {
			t.Fatalf("expected channel byte 1, got %d", data[0])
		}
		got = append(got, data[1:]...)
	}
	if !bytes.Equal(got, pack) {
		t.Fatalf("reassembled data length %d != %d", len(got), len(pack))
	}
}

// ---------------------------------------------------------------------------
// serveInfoRefs
// ---------------------------------------------------------------------------

func TestServeInfoRefs(t *testing.T) {
	repo := &memRepo{head: strings.Repeat("a", 40)}

	w := httptest.NewRecorder()
	serveInfoRefs(w, repo)

	if ct := w.Header().Get("Content-Type"); ct != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("unexpected Content-Type: %s", ct)
	}

	body := w.Body
	lines := readAllPktLines(body) // reads until first flush
	if len(lines) != 1 || string(lines[0]) != "# service=git-upload-pack\n" {
		t.Fatalf("unexpected service line: %q", lines)
	}

	// Second section: HEAD ref + refs/heads/main, then flush
	lines = readAllPktLines(body)
	if len(lines) != 2 {
		t.Fatalf("expected 2 ref lines, got %d: %q", len(lines), lines)
	}
	if !bytes.HasPrefix(lines[0], []byte(repo.head+" HEAD\x00")) {
		t.Fatalf("unexpected HEAD line: %q", lines[0])
	}
	if string(lines[1]) != repo.head+" refs/heads/main\n" {
		t.Fatalf("unexpected main line: %q", lines[1])
	}
}

// ---------------------------------------------------------------------------
// serveUploadPack
// ---------------------------------------------------------------------------

func TestServeUploadPack_ConsumesRepo(t *testing.T) {
	blobSHA, blobRaw := gitObject("blob", []byte("hi"))
	repo := &memRepo{
		objects: map[string][]byte{blobSHA: blobRaw},
		head:    blobSHA,
	}

	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(blobSHA))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if !repo.consumed {
		t.Fatal("repo should be marked consumed")
	}
}

func TestServeUploadPack_AlreadyConsumed(t *testing.T) {
	repo := &memRepo{consumed: true}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		io.NopCloser(bytes.NewReader(nil)))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", w.Code)
	}
}

func TestServeUploadPack_NAK(t *testing.T) {
	blobSHA, blobRaw := gitObject("blob", []byte("hi"))
	repo := &memRepo{
		objects: map[string][]byte{blobSHA: blobRaw},
		head:    blobSHA,
	}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(blobSHA))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	data, _, _ := readPktLine(w.Body)
	if string(data) != "NAK\n" {
		t.Fatalf("expected NAK, got %q", data)
	}
}

// ---------------------------------------------------------------------------
// getOrLoadRepo
// ---------------------------------------------------------------------------

func TestGetOrLoadRepo_NotFound(t *testing.T) {
	_, err := getOrLoadRepo("nonexistent-repo-that-does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing zip")
	}
}

func TestGetOrLoadRepo_LoadsAndCaches(t *testing.T) {
	t.Cleanup(func() {
		repoCache.Range(func(k, _ any) bool { repoCache.Delete(k); return true })
		os.Remove(uploadsDir + "/myrepo-test.zip") //nolint:errcheck
	})

	zipData := makeTestZip(map[string]string{"hello.txt": "hello"})
	if err := writeFile(uploadsDir+"/myrepo-test.zip", zipData); err != nil {
		t.Fatal(err)
	}

	r1, err := getOrLoadRepo("myrepo-test")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := getOrLoadRepo("myrepo-test")
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Fatal("expected same *memRepo instance from cache")
	}
}

// ---------------------------------------------------------------------------
// buildRepoFromZip
// ---------------------------------------------------------------------------

func TestBuildRepoFromZip_ObjectsPresent(t *testing.T) {
	zipData := makeTestZip(map[string]string{
		"hello.txt":    "hello",
		"sub/world.go": "package main",
	})
	repo, err := buildRepoFromZip(zipData)
	if err != nil {
		t.Fatal(err)
	}
	if repo.head == "" {
		t.Fatal("expected non-empty HEAD")
	}
	// Commit, root tree, sub tree, 2 blobs = at least 5 objects
	if len(repo.objects) < 5 {
		t.Fatalf("expected ≥5 objects, got %d", len(repo.objects))
	}
	// HEAD object must be a commit
	commitRaw := repo.objects[repo.head]
	if !bytes.HasPrefix(commitRaw, []byte("commit ")) {
		t.Fatalf("HEAD object is not a commit: %q", commitRaw[:20])
	}
}

func TestBuildRepoFromZip_InvalidZip(t *testing.T) {
	_, err := buildRepoFromZip([]byte("not a zip"))
	if err == nil {
		t.Fatal("expected error for invalid zip")
	}
}

// ---------------------------------------------------------------------------
// buildRepoFromZip — directory entry skipping
// ---------------------------------------------------------------------------

func TestBuildRepoFromZip_SkipsDirEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Explicit directory entry (name ends with "/")
	zw.CreateHeader(&zip.FileHeader{Name: "emptydir/", Method: zip.Store}) //nolint:errcheck
	f, _ := zw.Create("hello.txt")
	f.Write([]byte("hi")) //nolint:errcheck
	zw.Close()             //nolint:errcheck

	repo, err := buildRepoFromZip(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// Only hello.txt should appear; the dir entry must be skipped.
	// Verify we got a usable repo with exactly one blob.
	blobs := 0
	for _, raw := range repo.objects {
		if bytes.HasPrefix(raw, []byte("blob ")) {
			blobs++
		}
	}
	if blobs != 1 {
		t.Fatalf("expected 1 blob, got %d", blobs)
	}
}

// ---------------------------------------------------------------------------
// buildTree — coverage of ignored-prefix and dedup branches
// ---------------------------------------------------------------------------

func TestBuildTree_IgnoresUnrelatedPaths(t *testing.T) {
	sha, _ := gitObject("blob", []byte("a"))
	files := map[string]string{
		"src/a.go": sha,
		"lib/b.go": sha, // does not match prefix "src/"
	}
	objects := map[string][]byte{}
	treeSHA, err := buildTree(files, "src/", objects)
	if err != nil {
		t.Fatal(err)
	}
	names := treeEntryNames(objects[treeSHA])
	if len(names) != 1 || names[0] != "a.go" {
		t.Fatalf("expected [a.go], got %v", names)
	}
}

func TestBuildTree_DeduplicatesSubdir(t *testing.T) {
	sha, _ := gitObject("blob", []byte("a"))
	// Two files under the same subdir — should produce one "src" tree entry.
	files := map[string]string{
		"src/a.go": sha,
		"src/b.go": sha,
	}
	objects := map[string][]byte{}
	rootSHA, err := buildTree(files, "", objects)
	if err != nil {
		t.Fatal(err)
	}
	names := treeEntryNames(objects[rootSHA])
	if len(names) != 1 || names[0] != "src" {
		t.Fatalf("expected [src], got %v", names)
	}
}

// ---------------------------------------------------------------------------
// readPktLine — invalid length
// ---------------------------------------------------------------------------

func TestReadPktLine_InvalidLength(t *testing.T) {
	// Length "0001" means total length 1, which is < 4 — invalid.
	_, _, err := readPktLine(bytes.NewReader([]byte("0001")))
	if err == nil {
		t.Fatal("expected error for invalid pkt-line length")
	}
}

func TestReadPktLine_InvalidHex(t *testing.T) {
	// Non-hex header causes fmt.Sscanf to fail.
	_, _, err := readPktLine(bytes.NewReader([]byte("gggg")))
	if err == nil {
		t.Fatal("expected error for non-hex pkt-line header")
	}
}

// ---------------------------------------------------------------------------
// handleGit — internal error and unknown suffix
// ---------------------------------------------------------------------------

func TestHandleGit_NotFound(t *testing.T) {
	// Zip file does not exist → os.IsNotExist → 404 from handleGit.
	req := httptest.NewRequest(http.MethodGet, "/gitonce/missing-repo-xyz.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleGit_NoDotGit(t *testing.T) {
	// URL path with no ".git" segment → 404.
	req := httptest.NewRequest(http.MethodGet, "/gitonce/norepo", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleGit_PathTraversal(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/gitonce/../../etc/passwd.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleGit_ConsumedRepo(t *testing.T) {
	// Repo already consumed → 410 from handleGit's consumed check.
	repo := &memRepo{head: strings.Repeat("b", 40), objects: map[string][]byte{}, consumed: true}
	repoCache.Store("consumedrepo", repo)
	t.Cleanup(func() { repoCache.Delete("consumedrepo") })

	req := httptest.NewRequest(http.MethodGet, "/gitonce/consumedrepo.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", w.Code)
	}
}

func TestHandleGit_InternalError(t *testing.T) {
	t.Cleanup(func() {
		repoCache.Range(func(k, _ any) bool { repoCache.Delete(k); return true })
		os.Remove(uploadsDir + "/badrepo-test.zip") //nolint:errcheck
	})
	// A readable file that is not a valid zip causes buildRepoFromZip to fail
	// with a non-IsNotExist error, hitting the 500 branch in handleGit.
	if err := writeFile(uploadsDir+"/badrepo-test.zip", []byte("not a zip")); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/gitonce/badrepo-test.git/info/refs?service=git-upload-pack", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleGit_UnknownSuffix(t *testing.T) {
	repo := &memRepo{head: strings.Repeat("a", 40), objects: map[string][]byte{}}
	repoCache.Store("sufixrepo", repo)
	t.Cleanup(func() { repoCache.Delete("sufixrepo") })

	req := httptest.NewRequest(http.MethodGet, "/gitonce/sufixrepo.git/unknown", nil)
	w := httptest.NewRecorder()
	handleGit(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// serveUploadPack — buildPack error path
// ---------------------------------------------------------------------------

func TestServeUploadPack_NoSideband(t *testing.T) {
	// Client requests no side-band-64k → pack sent raw (non-sideband path).
	blobSHA, blobRaw := gitObject("blob", []byte("data"))
	repo := &memRepo{
		objects: map[string][]byte{blobSHA: blobRaw},
		head:    blobSHA,
	}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(blobSHA)) // no caps → no side-band-64k
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Skip NAK pkt-line, then remaining bytes should start with PACK magic.
	readPktLine(w.Body)
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("PACK")) {
		t.Fatalf("expected raw PACK, got %q", w.Body.Bytes()[:min(len(w.Body.Bytes()), 8)])
	}
}

func TestServeUploadPack_RemoveFails(t *testing.T) {
	// Point zipPath at a non-empty directory so os.Remove fails with a
	// non-IsNotExist error, exercising the log.Printf branch.
	dir := t.TempDir()
	// Make dir non-empty so os.Remove returns ENOTEMPTY.
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	blobSHA, blobRaw := gitObject("blob", []byte("x"))
	repo := &memRepo{
		objects: map[string][]byte{blobSHA: blobRaw},
		head:    blobSHA,
		zipPath: dir,
	}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(blobSHA))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestServeUploadPack_KeepUploads(t *testing.T) {
	// When keepUploads is true the zip file must not be deleted.
	orig := keepUploads
	keepUploads = true
	t.Cleanup(func() { keepUploads = orig })

	zipPath := filepath.Join(t.TempDir(), "keep.zip")
	if err := os.WriteFile(zipPath, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	blobSHA, blobRaw := gitObject("blob", []byte("x"))
	repo := &memRepo{
		objects: map[string][]byte{blobSHA: blobRaw},
		head:    blobSHA,
		zipPath: zipPath,
	}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(blobSHA))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("zip file should still exist, got: %v", err)
	}
}

func TestServeUploadPack_BuildPackError(t *testing.T) {
	// A raw object with no NUL separator causes buildPack to return an error.
	repo := &memRepo{
		objects: map[string][]byte{"abc": []byte("no-null-byte")},
		head:    strings.Repeat("a", 40),
	}
	req := httptest.NewRequest(http.MethodPost, "/git-upload-pack",
		uploadPackRequest(repo.head))
	w := httptest.NewRecorder()
	serveUploadPack(w, req, repo)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// writeFile is a small helper used only in tests.
func writeFile(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
