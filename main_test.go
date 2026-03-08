package main

import (
	"archive/zip"
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHandleCheck(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/check", nil)
	w := httptest.NewRecorder()
	handleCheck(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK\n" {
		t.Fatalf("expected body %q, got %q", "OK\n", w.Body.String())
	}
}

func TestHandleIndex(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("static", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("static/index.html", []byte("<html>hello</html>"), 0644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handleIndex(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
}

func TestHandleUpload_ParseFails(t *testing.T) {
	t.Chdir(t.TempDir())
	// Sending plain JSON (not multipart) causes ParseMultipartForm to fail → 413.
	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleUpload(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

func TestHandleUpload_MissingField(t *testing.T) {
	t.Chdir(t.TempDir())
	// Multipart body with no "zipfile" field → 400.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.CreateFormField("other") //nolint:errcheck
	mw.Close()                  //nolint:errcheck

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	handleUpload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpload_NotZip(t *testing.T) {
	t.Chdir(t.TempDir())

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("zipfile", "test.txt")
	part.Write([]byte("not a zip file")) //nolint:errcheck
	mw.Close()                           //nolint:errcheck

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	handleUpload(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleUpload_CreateFails(t *testing.T) {
	// t.Chdir to a dir without an uploads/ subdir so os.Create fails in saveUpload.
	t.Chdir(t.TempDir())

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("f.txt")
	f.Write([]byte("x")) //nolint:errcheck
	zw.Close()            //nolint:errcheck

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("zipfile", "test.zip")
	part.Write(zipBuf.Bytes()) //nolint:errcheck
	mw.Close()                 //nolint:errcheck

	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	handleUpload(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}
