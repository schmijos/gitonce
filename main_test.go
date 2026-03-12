package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLogRequests(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	handler := logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest(http.MethodGet, "/some/path?q=1", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	logged := buf.String()
	if !strings.Contains(logged, "GET /some/path?q=1") {
		t.Fatalf("expected log line with method and URL, got: %q", logged)
	}
	if !strings.Contains(logged, "418") {
		t.Fatalf("expected log line with status code, got: %q", logged)
	}
}

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

func TestHandleUpload_SchemeFromForwardedProto(t *testing.T) {
	t.Chdir(t.TempDir())

	makeZipBody := func(t *testing.T) (*bytes.Buffer, string) {
		t.Helper()
		var zipBuf bytes.Buffer
		zw := zip.NewWriter(&zipBuf)
		f, _ := zw.Create("file.txt")
		f.Write([]byte("data")) //nolint:errcheck
		zw.Close()              //nolint:errcheck

		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, _ := mw.CreateFormFile("zipfile", "test.zip")
		part.Write(zipBuf.Bytes()) //nolint:errcheck
		mw.Close()                         //nolint:errcheck
		return &body, mw.FormDataContentType()
	}

	t.Run("no TLS no header gives http", func(t *testing.T) {
		t.Chdir(t.TempDir())
		body, ct := makeZipBody(t)
		req := httptest.NewRequest(http.MethodPost, "/upload", body)
		req.Header.Set("Content-Type", ct)
		w := httptest.NewRecorder()
		handleUpload(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var result map[string]string
		json.NewDecoder(w.Body).Decode(&result) //nolint:errcheck
		if !strings.HasPrefix(result["url"], "http://") {
			t.Fatalf("expected http:// URL, got %q", result["url"])
		}
	})

	t.Run("X-Forwarded-Proto https gives https URL", func(t *testing.T) {
		t.Chdir(t.TempDir())
		body, ct := makeZipBody(t)
		req := httptest.NewRequest(http.MethodPost, "/upload", body)
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		handleUpload(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var result map[string]string
		json.NewDecoder(w.Body).Decode(&result) //nolint:errcheck
		if !strings.HasPrefix(result["url"], "https://") {
			t.Fatalf("expected https:// URL, got %q", result["url"])
		}
	})
}

