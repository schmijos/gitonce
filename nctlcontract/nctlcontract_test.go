// Package nctlcontract checks that the real nctl client (api/gitonce) and a
// Deploio-style clone work against the gitonce binary as built from this repo.
package nctlcontract

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6/memfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/storage/memory"
	"github.com/ninech/nctl/api/gitonce"
)

func TestNctlUploadThenClone(t *testing.T) {
	base := startGitonce(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := gitonce.UploadDirectory(context.Background(), dir, base+"/upload")
	if err != nil {
		t.Fatalf("nctl upload: %v", err)
	}
	if !strings.HasPrefix(res.URL, base+"/gitonce/") || !strings.HasSuffix(res.URL, ".git") {
		t.Fatalf("unexpected repo url %q", res.URL)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(res.Commit) {
		t.Fatalf("unexpected commit %q", res.Commit)
	}

	fs := memfs.New()
	repo, err := gogit.Clone(memory.NewStorage(), fs, &gogit.CloneOptions{URL: res.URL, Depth: 1})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash().String() != res.Commit {
		t.Fatalf("HEAD %s, nctl was told %s", head.Hash(), res.Commit)
	}
	f, err := fs.Open("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(f)
	f.Close()
	if string(content) != "hello" {
		t.Fatalf("hello.txt = %q", content)
	}
	if _, err := gogit.Clone(memory.NewStorage(), memfs.New(), &gogit.CloneOptions{URL: res.URL}); err == nil {
		t.Fatal("second clone must fail")
	}
}

// Buildpacks need bin/* and gradlew executable. Passes once the nctl client
// writes file modes into the zip.
func TestNctlUploadKeepsExecutableBit(t *testing.T) {
	base := startGitonce(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := gitonce.UploadDirectory(context.Background(), dir, base+"/upload")
	if err != nil {
		t.Fatalf("nctl upload: %v", err)
	}
	fs := memfs.New()
	if _, err := gogit.Clone(memory.NewStorage(), fs, &gogit.CloneOptions{URL: res.URL}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	st, err := fs.Stat("run")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&0o111 == 0 {
		t.Fatal("run lost its executable bit")
	}
}

// startGitonce builds the binary from the parent module and runs it on a free
// port. It returns the base URL.
func startGitonce(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gitonce")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	var logs bytes.Buffer
	cmd := exec.Command(bin)
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "KEEP_UPLOADS=")
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		if t.Failed() {
			t.Logf("gitonce output:\n%s", logs.String())
		}
	})

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/check"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gitonce did not become ready:\n%s", logs.String())
	return ""
}
