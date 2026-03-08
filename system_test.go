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
	"testing"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/go-git/go-billy/v6/memfs"
)

func TestUploadFormField(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatal(err)
	}

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
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		t.Fatal(err)
	}
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
	entries, _ := os.ReadDir(uploadsDir)
	if len(entries) != 0 {
		t.Fatalf("expected uploads dir to be empty after clone, found %d file(s)", len(entries))
	}
}
