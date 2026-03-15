package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/go-git/go-billy/v6/memfs"
)

// uploadZip uploads a zip to the test server and returns the repo URL and HEAD commit SHA.
func uploadZip(t *testing.T, srv *httptest.Server, zipData []byte) (repoURL, commitSHA string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("zipfile", "test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(zipData); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(srv.URL+"/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: got %d – %s", resp.StatusCode, b)
	}
	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	repoURL = result["url"]
	commitSHA = result["commit"]
	if repoURL == "" || commitSHA == "" {
		t.Fatalf("upload response missing url or commit: %v", result)
	}
	return
}

func TestUploadFormField(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("hello.txt")
	f.Write([]byte("hello"))
	zw.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("zipfile", "test.zip")
	part.Write(zipBuf.Bytes())
	mw.Close()

	resp, err := http.Post(srv.URL+"/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: got %d – %s", resp.StatusCode, b)
	}
}

func TestUploadAndClone(t *testing.T) {
	t.Cleanup(func() {
		repoCache.Range(func(k, _ any) bool { repoCache.Delete(k); return true })
	})

	srv := httptest.NewServer(newMux())
	defer srv.Close()

	// --- build a zip containing a single text file ---
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	fw, err := zw.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	// --- upload ---
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("zipfile", "test.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(zipBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(srv.URL+"/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: got %d – %s", resp.StatusCode, b)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	repoURL := result["url"]
	if repoURL == "" {
		t.Fatal("no url in upload response")
	}
	if result["commit"] == "" {
		t.Fatal("no commit in upload response")
	}

	// Derive the zip path from the repo URL: .../gitonce/<name>.git → /tmp/<name>.zip
	name := strings.TrimSuffix(path.Base(repoURL), ".git")
	zipPath := uploadsDir + "/" + name + ".zip"

	// --- clone via git smart HTTP ---
	_, err = gogit.Clone(memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: repoURL,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// --- verify the repo is now consumed (second clone must fail) ---
	_, err = gogit.Clone(memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: repoURL,
	})
	if err == nil {
		t.Fatal("expected second clone to fail, but it succeeded")
	}

	// --- verify zip was deleted after download ---
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("expected zip %s to be deleted after clone", zipPath)
	}
}

// TestKpackClonePattern simulates the exact HTTP request sequence that kpack
// (Kubernetes Native Container Build Service) uses when cloning a git repo:
// two GETs to /info/refs followed by one POST to /git-upload-pack with
// side-band-64k requested. This pattern differs from vanilla git which does
// a single GET + POST.
func TestKpackClonePattern(t *testing.T) {
	t.Cleanup(func() {
		repoCache.Range(func(k, _ any) bool { repoCache.Delete(k); return true })
	})

	srv := httptest.NewServer(newMux())
	defer srv.Close()

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	fw, _ := zw.Create("hello.txt")
	fw.Write([]byte("hello kpack"))
	zw.Close()

	repoURL, commitSHA := uploadZip(t, srv, zipBuf.Bytes())
	infoRefsURL := repoURL + "/info/refs?service=git-upload-pack"
	uploadPackURL := repoURL + "/git-upload-pack"

	// --- first GET /info/refs (kpack probes the repo) ---
	resp1, err := http.Get(infoRefsURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first info/refs: got %d", resp1.StatusCode)
	}
	// Content-Length must be set so ingress controllers do not strip the
	// capability advertisement (causing the client to omit side-band-64k).
	if resp1.ContentLength < 0 {
		t.Fatal("first info/refs: missing Content-Length")
	}

	// --- second GET /info/refs (kpack fetches refs again before cloning) ---
	resp2, err := http.Get(infoRefsURL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second info/refs: got %d", resp2.StatusCode)
	}
	if resp2.ContentLength < 0 {
		t.Fatal("second info/refs: missing Content-Length")
	}

	// --- POST /git-upload-pack with side-band-64k + deepen (real kpack request) ---
	// kpack's go-git always includes "deepen 1" for shallow clones.
	var bodyBuf bytes.Buffer
	bodyBuf.Write(pktLine([]byte("want " + commitSHA + " side-band-64k ofs-delta\n")))
	bodyBuf.Write(pktLine([]byte("deepen 1\n")))
	bodyBuf.Write(pktFlush)
	bodyBuf.Write(pktLine([]byte("done\n")))

	postResp, err := http.Post(uploadPackURL, "application/x-git-upload-pack-request", &bodyBuf)
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(postResp.Body)
		t.Fatalf("upload-pack: got %d – %s", postResp.StatusCode, b)
	}

	// When deepen was requested, the server must send shallow <sha> + flush
	// before NAK. Without this, go-git's shallow-update parser consumes the
	// NAK pkt-line and the server-response reader never sees it.
	shallow, flush, err := readPktLine(postResp.Body)
	if err != nil {
		t.Fatalf("reading shallow line: %v", err)
	}
	if flush {
		t.Fatal("expected shallow line, got flush")
	}
	if !bytes.HasPrefix(shallow, []byte("shallow ")) {
		t.Fatalf("expected shallow line, got %q", shallow)
	}
	if !bytes.HasSuffix(bytes.TrimRight(shallow, "\n"), []byte(commitSHA)) {
		t.Fatalf("shallow line has wrong SHA: %q", shallow)
	}
	_, flushAfterShallow, err := readPktLine(postResp.Body)
	if err != nil || !flushAfterShallow {
		t.Fatalf("expected flush after shallow lines: err=%v flush=%v", err, flushAfterShallow)
	}

	// Read NAK pkt-line.
	nak, flush, err := readPktLine(postResp.Body)
	if err != nil {
		t.Fatalf("reading NAK: %v", err)
	}
	if flush || !bytes.HasPrefix(nak, []byte("NAK")) {
		t.Fatalf("expected NAK, got flush=%v data=%q", flush, nak)
	}

	// Reassemble pack data from side-band channel-1 packets.
	var pack bytes.Buffer
	for {
		data, flush, err := readPktLine(postResp.Body)
		if err != nil {
			t.Fatalf("reading sideband: %v", err)
		}
		if flush {
			break
		}
		if len(data) == 0 {
			continue
		}
		channel := data[0]
		payload := data[1:]
		switch channel {
		case 1: // data
			pack.Write(payload)
		case 2: // progress — ignore
		case 3:
			t.Fatalf("sideband error: %s", payload)
		default:
			t.Fatalf("unexpected sideband channel %d", channel)
		}
	}

	packBytes := pack.Bytes()
	if len(packBytes) < 32 {
		t.Fatalf("pack too short: %d bytes", len(packBytes))
	}
	if !bytes.HasPrefix(packBytes, []byte("PACK")) {
		t.Fatalf("pack missing PACK magic, got %q", packBytes[:4])
	}
	if count := packObjectCount(packBytes); count == 0 {
		t.Fatal("pack contains zero objects")
	}
	if !packSHA1Valid(packBytes) {
		t.Fatal("pack SHA1 checksum invalid")
	}

	// --- repo must be consumed; repeat POST must return 410 Gone ---
	var body2 bytes.Buffer
	io.Copy(&body2, uploadPackRequest(commitSHA, "side-band-64k"))
	resp3, err := http.Post(uploadPackURL, "application/x-git-upload-pack-request", &body2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusGone {
		t.Fatalf("expected 410 Gone on second clone, got %d", resp3.StatusCode)
	}
}
