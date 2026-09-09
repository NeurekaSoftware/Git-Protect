package lfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func pointerFor(content []byte) (string, string) {
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	pointerText := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(content))
	return oid, pointerText
}

// newRepoWithLFS creates a working-copy repository committing the given files
// (name → content). LFS-pointer files should already be pointer text.
func newRepoWithLFS(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repository, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := worktree.Commit("add files", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeLFSServer implements enough of the LFS batch protocol for the tests.
type fakeLFSServer struct {
	server     *httptest.Server
	mu         sync.Mutex
	data       map[string][]byte
	batchCalls int
	downloads  map[string]int
	// batchStatus, when non-zero, is the status returned for batch requests.
	batchStatus int
	// corruptDownload serves wrong bytes for every object.
	corruptDownload bool
	// batchPath overrides the expected batch path (lfs.url override tests).
	batchPath string
}

func newFakeLFSServer(t *testing.T, data map[string][]byte) *fakeLFSServer {
	t.Helper()
	s := &fakeLFSServer{data: data, downloads: make(map[string]int)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/objects/batch"):
			s.handleBatch(w, r)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			s.handleDownload(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// remoteURL is the remote a repository would use to reach the fake server.
func (s *fakeLFSServer) remoteURL() string {
	return s.server.URL + "/repo.git"
}

func (s *fakeLFSServer) handleBatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.batchCalls++
	status, batchPath := s.batchStatus, s.batchPath
	s.mu.Unlock()

	if batchPath != "" && r.URL.Path != batchPath {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}

	var request batchRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	response := batchResponse{Objects: make([]batchResponseObject, 0, len(request.Objects))}
	for _, object := range request.Objects {
		if _, known := s.data[object.OID]; !known {
			response.Objects = append(response.Objects, batchResponseObject{
				OID:   object.OID,
				Error: &batchError{Code: http.StatusNotFound, Message: "Object does not exist"},
			})
			continue
		}
		response.Objects = append(response.Objects, batchResponseObject{
			OID:     object.OID,
			Size:    int64(len(s.data[object.OID])),
			Actions: map[string]*batchAction{"download": {Href: s.server.URL + "/download/" + object.OID}},
		})
	}
	w.Header().Set("Content-Type", lfsMediaType)
	_ = json.NewEncoder(w).Encode(response)
}

func (s *fakeLFSServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	oid := strings.TrimPrefix(r.URL.Path, "/download/")
	s.mu.Lock()
	s.downloads[oid]++
	content, known := s.data[oid]
	corrupt := s.corruptDownload
	s.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if corrupt {
		content = []byte("corrupted!")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(content)
}

func (s *fakeLFSServer) batchCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batchCalls
}

func (s *fakeLFSServer) downloadCount(oid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.downloads[oid]
}

func TestParsePointer(t *testing.T) {
	content := []byte{1, 2, 3}
	oid, pointerText := pointerFor(content)

	parsed, ok := parsePointer([]byte(pointerText))
	if !ok {
		t.Fatal("valid pointer should parse")
	}
	if parsed.oid != oid || parsed.size != int64(len(content)) {
		t.Fatalf("parsed = %+v", parsed)
	}

	invalid := map[string]string{
		"missing version":  fmt.Sprintf("oid sha256:%s\nsize 3\n", strings.Repeat("a", 64)),
		"wrong version":    "version https://example.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 3\n",
		"missing oid":      "version https://git-lfs.github.com/spec/v1\nsize 3\n",
		"missing size":     "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\n",
		"short oid":        "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 3\n",
		"non-hex oid":      "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("g", 64) + "\nsize 3\n",
		"negative size":    "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize -1\n",
		"not a key value":  "just some text\n",
		"binary gibberish": string([]byte{0, 1, 2, 3}),
	}
	for name, text := range invalid {
		if _, ok := parsePointer([]byte(text)); ok {
			t.Errorf("%s should not parse", name)
		}
	}

	// Unknown extension fields are allowed by the pointer spec.
	extended := pointerText + "x-anything custom-value\n"
	if _, ok := parsePointer([]byte(extended)); !ok {
		t.Error("pointer with extension fields should parse")
	}
}

func TestFetchAllDownloadsAndCaches(t *testing.T) {
	content := []byte("large model weights \x00\x01\x02")
	oid, pointerText := pointerFor(content)
	lfsServer := newFakeLFSServer(t, map[string][]byte{oid: content})

	repositoryPath := newRepoWithLFS(t, map[string]string{
		"models/weights.bin": pointerText,
		"README.md":          "no LFS here",
	})

	fetcher := NewFetcher()
	if err := fetcher.FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "user", "pass"); err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}

	cached, err := os.ReadFile(filepath.Join(repositoryPath, ".git", "lfs", "objects", oid[0:2], oid[2:4], oid))
	if err != nil {
		t.Fatalf("LFS object not cached in the git-lfs layout: %v", err)
	}
	if string(cached) != string(content) {
		t.Error("cached content should match the served bytes")
	}
	if lfsServer.downloadCount(oid) != 1 {
		t.Errorf("download count = %d, want 1", lfsServer.downloadCount(oid))
	}

	// A repeated fetch must not re-download the cached object.
	if err := fetcher.FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "user", "pass"); err != nil {
		t.Fatalf("second FetchAll failed: %v", err)
	}
	if lfsServer.downloadCount(oid) != 1 {
		t.Errorf("cached object was downloaded again (count=%d)", lfsServer.downloadCount(oid))
	}
}

func TestFetchAllSkipsEndpointWhenNoPointers(t *testing.T) {
	lfsServer := newFakeLFSServer(t, nil)
	repositoryPath := newRepoWithLFS(t, map[string]string{"README.md": "plain repo"})

	if err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", ""); err != nil {
		t.Fatalf("FetchAll failed: %v", err)
	}
	if lfsServer.batchCallCount() != 0 {
		t.Errorf("batch endpoint should not be contacted without pointers (calls=%d)", lfsServer.batchCallCount())
	}
}

func TestFetchAllDisabledRemote(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
		lfsServer.batchStatus = status
		repositoryPath := newRepoWithLFS(t, map[string]string{"file.bin": pointerText})

		err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", "")
		if !errors.Is(err, ErrDisabled) {
			t.Errorf("status %d should map to ErrDisabled, got %v", status, err)
		}
	}
}

func TestFetchAllUnauthorizedIsNotDisabled(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
	lfsServer.batchStatus = http.StatusUnauthorized
	repositoryPath := newRepoWithLFS(t, map[string]string{"file.bin": pointerText})

	err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", "")
	if err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("401 should be a genuine error, got %v", err)
	}
}

func TestFetchAllHashMismatch(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
	lfsServer.corruptDownload = true
	repositoryPath := newRepoWithLFS(t, map[string]string{"file.bin": pointerText})

	err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", "")
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("corrupt download should fail with a hash mismatch, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(repositoryPath, ".git", "lfs", "objects", oid[0:2], oid[2:4], oid)); !os.IsNotExist(statErr) {
		t.Error("corrupted download must not be stored")
	}
}

func TestFetchAllHonorsLFSConfigOverride(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
	lfsServer.batchPath = "/custom/lfs/objects/batch"

	repositoryPath := newRepoWithLFS(t, map[string]string{
		".lfsconfig": "[lfs]\n\turl = " + lfsServer.server.URL + "/custom/lfs\n",
		"file.bin":   pointerText,
	})

	if err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", ""); err != nil {
		t.Fatalf("FetchAll with .lfsconfig override failed: %v", err)
	}
}

func TestFetchAllUnknownObjectFails(t *testing.T) {
	_, pointerText := pointerFor([]byte("content"))
	lfsServer := newFakeLFSServer(t, nil) // knows nothing
	repositoryPath := newRepoWithLFS(t, map[string]string{"file.bin": pointerText})

	err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "", "")
	if err == nil || errors.Is(err, ErrDisabled) {
		t.Fatalf("unknown object should be a genuine error, got %v", err)
	}
}

func TestFetchAllRejectsUnsafeLFSConfigOverrides(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	cases := []struct {
		name   string
		lfsURL string
	}{
		{"off-host override", "https://evil.example.com/lfs"},
		{"non-http scheme", "ssh://git@evil.example.com/repo.git/lfs"},
		{"relative value", "/custom/lfs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
			repositoryPath := newRepoWithLFS(t, map[string]string{
				".lfsconfig": "[lfs]\n\turl = " + c.lfsURL + "\n",
				"file.bin":   pointerText,
			})

			err := NewFetcher().FetchAll(context.Background(), repositoryPath, lfsServer.remoteURL(), "user", "pass")
			if err == nil || !strings.Contains(err.Error(), "lfs.url") {
				t.Fatalf("err = %v, want it to name the lfs.url override", err)
			}
			if lfsServer.batchCallCount() != 0 {
				t.Error("the batch endpoint must not be contacted for an unsafe lfs.url override")
			}
		})
	}
}

func TestFetchAllRejectsNonHTTPRemoteURL(t *testing.T) {
	repositoryPath := newRepoWithLFS(t, map[string]string{"README.md": "plain repo"})

	err := NewFetcher().FetchAll(context.Background(), repositoryPath, "ftp://example.com/repo.git", "", "")
	if err == nil || !strings.Contains(err.Error(), "http and https") {
		t.Fatalf("err = %v, want an http/https rejection", err)
	}
}

func TestCanonicalHostStripsSchemeDefaultPorts(t *testing.T) {
	mustParse := func(raw string) *url.URL {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return parsed
	}

	if canonicalHost(mustParse("https://Example.com:443/lfs")) != canonicalHost(mustParse("https://example.com/lfs")) {
		t.Error("an explicit https default port must equal the implicit default")
	}
	if canonicalHost(mustParse("http://example.com:80/lfs")) != "example.com" {
		t.Errorf("http default port = %q, want example.com", canonicalHost(mustParse("http://example.com:80/lfs")))
	}
	if canonicalHost(mustParse("https://example.com:8443/lfs")) == canonicalHost(mustParse("https://example.com/lfs")) {
		t.Error("a non-default port must stay distinct from the bare host")
	}
}

func TestResolveEndpointOverrideHostAndScheme(t *testing.T) {
	oid, pointerText := pointerFor([]byte("content"))
	cases := []struct {
		name        string
		lfsURL      string
		remoteURL   string
		wantReject  bool
		wantAddress string
	}{
		{
			name:        "explicit default port matches bare remote host",
			lfsURL:      "https://example.com:443/custom/lfs",
			remoteURL:   "https://example.com/repo.git",
			wantAddress: "https://example.com/custom/lfs",
		},
		{
			name:       "http override downgrades the https remote",
			lfsURL:     "http://example.com/custom/lfs",
			remoteURL:  "https://example.com/repo.git",
			wantReject: true,
		},
		{
			name:       "foreign port on the remote host",
			lfsURL:     "https://example.com:8443/custom/lfs",
			remoteURL:  "https://example.com/repo.git",
			wantReject: true,
		},
		{
			name:       "off-host override",
			lfsURL:     "https://evil.example.com/lfs",
			remoteURL:  "https://example.com/repo.git",
			wantReject: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lfsServer := newFakeLFSServer(t, map[string][]byte{oid: []byte("content")})
			repositoryPath := newRepoWithLFS(t, map[string]string{
				".lfsconfig": "[lfs]\n\turl = " + c.lfsURL + "\n",
				"file.bin":   pointerText,
			})
			repository, err := git.PlainOpen(repositoryPath)
			if err != nil {
				t.Fatal(err)
			}

			address, err := resolveEndpoint(repository, c.remoteURL)
			if c.wantReject {
				if err == nil {
					t.Fatalf("resolveEndpoint = %q, want rejection", address)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveEndpoint failed: %v", err)
			}
			if address != c.wantAddress {
				t.Errorf("endpoint = %q, want %q", address, c.wantAddress)
			}
			if lfsServer.batchCallCount() != 0 {
				t.Error("resolveEndpoint must not contact any endpoint")
			}
		})
	}
}
