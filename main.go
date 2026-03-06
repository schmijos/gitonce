package main

import (
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("/git/", handleGit)

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	// TODO: receive zip upload, extract, init git repo
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func handleGit(w http.ResponseWriter, r *http.Request) {
	// TODO: proxy to git http-backend for smart HTTP git serving
	http.Error(w, "not implemented", http.StatusNotImplemented)
}