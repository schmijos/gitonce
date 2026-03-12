package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultPort = "8080"
const maxUploadSize = 50 << 20 // 50 MB
const uploadsDir = "/tmp"

func newMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /check", handleCheck)

    // Upload page:
	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("POST /upload", handleUpload)

	// Fake Git:
	mux.HandleFunc("GET /gitonce/", handleGit)
	mux.HandleFunc("POST /gitonce/", handleGit)

	return mux
}

func main() {
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		log.Fatal(err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           logRequests(newMux()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d", r.Method, r.URL, rec.status)
	})
}

func handleCheck(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK\n")) //nolint:errcheck
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file too large (max 50MB)", http.StatusRequestEntityTooLarge)
		return
	}

	file, _, err := r.FormFile("zipfile")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	var magic [4]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil || magic != [4]byte{'P', 'K', 0x03, 0x04} {
		http.Error(w, "uploaded file is not a zip", http.StatusBadRequest)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	name, err := saveUpload(file)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	repo, err := getOrLoadRepo(name)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{
		"message": "upload successful",
		"url":     fmt.Sprintf("%s://%s/gitonce/%s.git", scheme, r.Host, name),
		"commit":  repo.head,
	}); err != nil {
		log.Printf("json encode: %v", err)
	}
}

func saveUpload(src io.Reader) (name string, err error) {
	var randBytes [8]byte
	if _, err = rand.Read(randBytes[:]); err != nil {
		return
	}
	name = fmt.Sprintf("%d-%s", time.Now().Unix(), hex.EncodeToString(randBytes[:]))

	dst, err := os.Create(filepath.Join(uploadsDir, name+".zip"))
	if err != nil {
		return
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return
}
