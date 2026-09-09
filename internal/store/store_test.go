package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
)

// putRecord captures one single-PutObject request.
type putRecord struct {
	key     string
	headers http.Header
	body    []byte
}

// multipartRecord captures one full multipart exchange.
type multipartRecord struct {
	key        string
	initiateCT string
	uploadID   string
	parts      map[int][]byte
	complete   bool
	partOrder  []int
}

// fakeS3 is a minimal S3-compatible endpoint exercising exactly the surface
// the storage layer uses: PutObject, multipart, ListObjectsV2, and deletes.
type fakeS3 struct {
	server *httptest.Server

	mu           sync.Mutex
	puts         []putRecord
	multiparts   []*multipartRecord
	listPrefixes []string
	listed       []string
	listPageSize int
	deleteCalls  [][]string
	// deleteFailures maps the 1-based delete-call number to a status that
	// should be returned instead of success.
	deleteFailures map[int]int
	// putFailures is a countdown of transient 503s to return for puts.
	putFailures int
	// putAttempts counts every PutObject request, including failures.
	putAttempts int
}

func newFakeS3(t *testing.T) *fakeS3 {
	t.Helper()
	fake := &fakeS3{listPageSize: 3, deleteFailures: make(map[int]int)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", fake.serve)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeS3) settings() config.Storage {
	return config.Storage{
		Endpoint:        f.server.URL,
		Region:          "test-region",
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
		Bucket:          "test-bucket",
	}
}

func (f *fakeS3) newStorage(t *testing.T) *ObjectStorage {
	t.Helper()
	storage, err := NewObjectStorage(f.settings())
	if err != nil {
		t.Fatal(err)
	}
	return storage
}

// objectKey strips the leading bucket segment for path-style requests;
// virtual-host-style requests (or IP-host endpoints) have no bucket segment.
func (f *fakeS3) objectKey(r *http.Request) string {
	p := strings.TrimPrefix(r.URL.Path, "/")
	return strings.TrimPrefix(p, "test-bucket/")
}

// operation identifies the S3 operation. The SDK usually sends an x-id query
// parameter, but DeleteObjects (and any endpoint form it picks) can arrive
// without one, so fall back to the well-known query markers.
func (f *fakeS3) operation(r *http.Request) string {
	if op := r.URL.Query().Get("x-id"); op != "" {
		return op
	}
	query := r.URL.Query()
	switch {
	case query.Has("delete"):
		return "DeleteObjects"
	case query.Get("partNumber") != "":
		return "UploadPart"
	case query.Get("uploadId") != "":
		return "CompleteMultipartUpload"
	case query.Has("uploads"):
		return "CreateMultipartUpload"
	case query.Get("list-type") == "2":
		return "ListObjectsV2"
	default:
		return "unknown:" + r.Method
	}
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch f.operation(r) {
	case "PutObject":
		f.putAttempts++
		if f.putFailures > 0 {
			f.putFailures--
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `<Error><Code>ServiceUnavailable</Code><Message>slow down</Message></Error>`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.puts = append(f.puts, putRecord{key: f.objectKey(r), headers: r.Header.Clone(), body: body})
		w.WriteHeader(http.StatusOK)

	case "DeleteObject":
		f.deleteCalls = append(f.deleteCalls, []string{f.objectKey(r)})
		w.WriteHeader(http.StatusNoContent)

	case "CreateMultipartUpload":
		record := &multipartRecord{
			key:        f.objectKey(r),
			initiateCT: r.Header.Get("Content-Type"),
			parts:      make(map[int][]byte),
		}
		f.multiparts = append(f.multiparts, record)
		record.uploadID = fmt.Sprintf("upload-%d", len(f.multiparts))
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>test-bucket</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, record.key, record.uploadID)

	case "UploadPart":
		uploadID := r.URL.Query().Get("uploadId")
		var partNumber int
		_, _ = fmt.Sscanf(r.URL.Query().Get("partNumber"), "%d", &partNumber)
		body, _ := io.ReadAll(r.Body)
		for _, record := range f.multiparts {
			if record.uploadID == uploadID {
				record.parts[partNumber] = body
				record.partOrder = append(record.partOrder, partNumber)
			}
		}
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, partNumber))
		w.WriteHeader(http.StatusOK)

	case "CompleteMultipartUpload":
		uploadID := r.URL.Query().Get("uploadId")
		for _, record := range f.multiparts {
			if record.uploadID == uploadID {
				record.complete = true
			}
		}
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>http://test</Location><Bucket>test-bucket</Bucket><Key>key</Key><ETag>"final"</ETag></CompleteMultipartUploadResult>`)

	case "DeleteObjects":
		var request struct {
			Objects []struct {
				Key string `xml:"Key"`
			} `xml:"Object"`
		}
		_ = xml.NewDecoder(r.Body).Decode(&request)
		var keys []string
		for _, object := range request.Objects {
			keys = append(keys, object.Key)
		}
		callNumber := len(f.deleteCalls) + 1
		f.deleteCalls = append(f.deleteCalls, keys)
		if status, fail := f.deleteFailures[callNumber]; fail {
			w.WriteHeader(status)
			if status == http.StatusBadRequest {
				_, _ = fmt.Fprint(w, `<Error><Code>MalformedXML</Code><Message>The XML you provided was not well formed</Message></Error>`)
			} else {
				_, _ = fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>nope</Message></Error>`)
			}
			return
		}
		_, _ = fmt.Fprint(w, `<DeleteResult></DeleteResult>`)

	case "ListObjectsV2":
		prefix := r.URL.Query().Get("prefix")
		if len(f.listPrefixes) == 0 || f.listPrefixes[len(f.listPrefixes)-1] != prefix {
			f.listPrefixes = append(f.listPrefixes, prefix)
		}
		token := r.URL.Query().Get("continuation-token")
		start := 0
		if token != "" {
			_, _ = fmt.Sscanf(token, "%d", &start)
		}
		end := min(start+f.listPageSize, len(f.listed))
		response := &listBucketResult{IsTruncated: end < len(f.listed), MaxKeys: f.listPageSize}
		if response.IsTruncated {
			response.NextContinuationToken = fmt.Sprintf("%d", end)
		}
		for _, key := range f.listed[start:end] {
			response.Contents = append(response.Contents, &struct {
				Key string `xml:"Key"`
			}{Key: key})
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(response)

	default:
		fmt.Printf("FAKE-S3-UNHANDLED: %s %s?%s\n", r.Method, r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string   `xml:"Name"`
	Prefix                string   `xml:"Prefix"`
	KeyCount              int      `xml:"KeyCount"`
	MaxKeys               int      `xml:"MaxKeys"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
	Contents              []*struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

func TestResolveEndpoint(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		bucket    string
		forcePath bool
		want      string
		wantSDK   string
		wantPath  bool
	}{
		{
			name:     "virtual host",
			endpoint: "https://s3.example.com",
			bucket:   "my-bucket",
			want:     "https://{Bucket}.s3.example.com",
			wantSDK:  "https://s3.example.com",
			wantPath: false,
		},
		{
			name:     "virtual host with port and path",
			endpoint: "https://s3.example.com:8443/base",
			bucket:   "my-bucket",
			want:     "https://{Bucket}.s3.example.com:8443/base",
			wantSDK:  "https://s3.example.com:8443/base",
			wantPath: false,
		},
		{
			name:     "bucket-prefixed host is stripped",
			endpoint: "https://my-bucket.s3.example.com",
			bucket:   "my-bucket",
			want:     "https://{Bucket}.s3.example.com",
			wantSDK:  "https://s3.example.com",
			wantPath: false,
		},
		{
			name:      "path style keeps the endpoint",
			endpoint:  "https://s3.example.com",
			bucket:    "my-bucket",
			forcePath: true,
			want:      "https://s3.example.com",
			wantSDK:   "https://s3.example.com",
			wantPath:  true,
		},
		{
			name:     "user template is taken literally",
			endpoint: "https://{Bucket}.s3.example.com",
			bucket:   "my-bucket",
			want:     "https://{Bucket}.s3.example.com",
			wantSDK:  "https://my-bucket.s3.example.com",
			wantPath: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveEndpoint(tt.endpoint, tt.bucket, tt.forcePath)
			if got != tt.want {
				t.Errorf("resolveEndpoint = %q, want %q", got, tt.want)
			}
			sdkEndpoint, usePathStyle := sdkEndpointFor(tt.endpoint, tt.bucket, tt.forcePath)
			if sdkEndpoint != tt.wantSDK || usePathStyle != tt.wantPath {
				t.Errorf("sdkEndpointFor = %q, %v, want %q, %v", sdkEndpoint, usePathStyle, tt.wantSDK, tt.wantPath)
			}
		})
	}
}

func TestUploadTextGzipContent(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	content := `{"mode":"provider","repositoryUrl":"https://example.com/repo.git","updatedAtUnixSeconds":1700000000}`
	if err := storage.UploadText(context.Background(), "repositories/provider/github/o/r/metadata.json", content); err != nil {
		t.Fatalf("UploadText failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(fake.puts))
	}
	put := fake.puts[0]
	if put.key != "repositories/provider/github/o/r/metadata.json" {
		t.Errorf("key = %q", put.key)
	}
	if got := put.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := put.headers.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q", got)
	}

	reader, err := gzip.NewReader(bytes.NewReader(put.body))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(decompressed) != content {
		t.Errorf("decompressed content = %q, want %q", decompressed, content)
	}
}

func TestUploadStreamSmallSinglePut(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	content := []byte("screenshot bytes")
	err := storage.UploadStream(context.Background(), "attachments/1/img.png", bytes.NewReader(content), "image/png", int64(len(content)))
	if err != nil {
		t.Fatalf("UploadStream failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.puts) != 1 || len(fake.multiparts) != 0 {
		t.Fatalf("expected one single put, got puts=%d multiparts=%d", len(fake.puts), len(fake.multiparts))
	}
	put := fake.puts[0]
	if put.headers.Get("Content-Type") != "image/png" {
		t.Errorf("Content-Type = %q", put.headers.Get("Content-Type"))
	}
	if string(put.body) != string(content) {
		t.Error("body should be the raw attachment")
	}
}

func TestUploadStreamSmallUnknownLengthUsesMultipart(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	content := []byte("streamed attachment")
	err := storage.UploadStream(context.Background(), "attachments/1/img.png", bytes.NewReader(content), "image/png", -1)
	if err != nil {
		t.Fatalf("UploadStream failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.multiparts) != 1 {
		t.Fatalf("expected one multipart upload, got %d", len(fake.multiparts))
	}
	if fake.multiparts[0].initiateCT != "" {
		t.Errorf("multipart initiate carried Content-Type %q", fake.multiparts[0].initiateCT)
	}
	if !fake.multiparts[0].complete {
		t.Error("multipart upload was not completed")
	}
}

func TestUploadDirectoryAsTarGzStreamsMultipart(t *testing.T) {
	previousBase := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = previousBase })

	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	// Enough pseudo-random bytes to span more than one 16 MiB part.
	large := make([]byte, MultipartPartSizeBytes+5*1024*1024)
	rand.New(rand.NewSource(42)).Read(large)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "packed.pack"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "objects", "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "ab", "cdef"), []byte("loose object"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := storage.UploadDirectoryAsTarGz(context.Background(), dir, "repositories/provider/github/o/r/1700000000_repo.tar.gz"); err != nil {
		t.Fatalf("UploadDirectoryAsTarGz failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.multiparts) != 1 {
		t.Fatalf("expected one multipart upload, got %d", len(fake.multiparts))
	}
	record := fake.multiparts[0]
	if record.initiateCT != "" {
		t.Errorf("multipart initiate carried Content-Type %q", record.initiateCT)
	}
	if !record.complete {
		t.Fatal("multipart upload was not completed")
	}
	if len(record.parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(record.parts))
	}

	// The parts are fragments of one continuous gzip stream: concatenate the
	// raw bytes first, then decompress once.
	var rawStream bytes.Buffer
	for _, number := range []int{1, 2} {
		rawStream.Write(record.parts[number])
	}
	var tarball bytes.Buffer
	gzipReader, err := gzip.NewReader(&rawStream)
	if err != nil {
		t.Fatalf("stream is not gzip: %v", err)
	}
	if _, err := io.Copy(&tarball, gzipReader); err != nil {
		t.Fatal(err)
	}

	tarReader := tar.NewReader(&tarball)
	type entry struct {
		name    string
		content []byte
	}
	var entries []entry
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{name: header.Name, content: data})
	}

	var names []string
	for _, item := range entries {
		names = append(names, item.name)
	}
	// WalkDir visits the objects/ subtree (sorted) before packed.pack.
	wantNames := []string{"objects/", "objects/ab/", "objects/ab/cdef", "packed.pack"}
	if len(entries) != len(wantNames) {
		t.Fatalf("tar entries = %v, want %v", names, wantNames)
	}
	for i, want := range wantNames {
		if entries[i].name != want {
			t.Errorf("entries[%d].name = %q, want %q", i, entries[i].name, want)
		}
	}
	if !bytes.Equal(entries[3].content, large) {
		t.Errorf("packed.pack content differs (%d bytes, want %d)", len(entries[3].content), len(large))
	}
	if string(entries[2].content) != "loose object" {
		t.Errorf("loose object content differs")
	}
}

func TestListObjectKeysPaginatesAndNormalizesPrefix(t *testing.T) {
	fake := newFakeS3(t)
	fake.mu.Lock()
	fake.listed = []string{"repositories/url/a/1_repo.tar.gz", "repositories/url/a/metadata.json", "x", "y", "z"}
	fake.mu.Unlock()
	storage := fake.newStorage(t)

	keys, err := storage.ListObjectKeys(context.Background(), "/repositories/url/a/")
	if err != nil {
		t.Fatalf("ListObjectKeys failed: %v", err)
	}
	if len(keys) != 5 {
		t.Fatalf("keys = %v", keys)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.listPrefixes) == 0 || fake.listPrefixes[0] != "repositories/url/a/" {
		t.Errorf("requested prefixes = %v", fake.listPrefixes)
	}
}

func TestDeleteObjectsBatchesAndFallsBack(t *testing.T) {
	previousBase := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = previousBase })

	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	// 1001 keys: one full batch plus a remainder of one. The first bulk
	// attempt is rejected with the schema error, so all deletes fall back to
	// single-object calls (and stay there for the rest of the run).
	var keys []string
	for i := 0; i < 1001; i++ {
		keys = append(keys, fmt.Sprintf("repositories/url/a/%06d_repo.tar.gz", i))
	}
	fake.mu.Lock()
	fake.deleteFailures[1] = http.StatusBadRequest
	fake.mu.Unlock()

	if err := storage.DeleteObjects(context.Background(), keys); err != nil {
		t.Fatalf("DeleteObjects failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Call 1 is the rejected bulk batch; the 1001 keys follow as single calls.
	if len(fake.deleteCalls) != 1002 {
		t.Fatalf("delete calls = %d, want 1002 (1 bulk + 1001 single)", len(fake.deleteCalls))
	}
	if len(fake.deleteCalls[0]) != 1000 {
		t.Fatalf("first call = %d keys, want the 1000-key bulk batch", len(fake.deleteCalls[0]))
	}
	for _, call := range fake.deleteCalls[1:] {
		if len(call) != 1 {
			t.Fatalf("expected single-object deletes after the fallback, got a call with %d keys", len(call))
		}
	}
}

func TestDeleteObjectsIgnoresBlankAndDuplicateKeys(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	err := storage.DeleteObjects(context.Background(), []string{"", "  ", "/a/b/", "a/b", "a/b"})
	if err != nil {
		t.Fatalf("DeleteObjects failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleteCalls) != 1 || len(fake.deleteCalls[0]) != 1 || fake.deleteCalls[0][0] != "a/b" {
		t.Fatalf("delete calls = %+v", fake.deleteCalls)
	}
}

func TestDeleteObjectsNoneNeeded(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)
	if err := storage.DeleteObjects(context.Background(), nil); err != nil {
		t.Fatalf("DeleteObjects failed: %v", err)
	}
}

func TestStorageFailureReportsDomainError(t *testing.T) {
	previousBase := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = previousBase })

	fake := newFakeS3(t)
	storage := fake.newStorage(t)
	fake.mu.Lock()
	fake.deleteFailures[1] = http.StatusForbidden // not retryable, not a schema error
	fake.mu.Unlock()

	err := storage.DeleteObjects(context.Background(), []string{"a/b"})
	if err == nil || !strings.Contains(err.Error(), "storage: failed to delete objects") {
		t.Fatalf("DeleteObjects error = %v", err)
	}
}

func TestUploadTextRejectsBlankKey(t *testing.T) {
	fake := newFakeS3(t)
	storage := fake.newStorage(t)
	if err := storage.UploadText(context.Background(), "///", "content"); err == nil {
		t.Fatal("blank object key should be rejected")
	}
}

func TestResolveContentType(t *testing.T) {
	tests := []struct {
		fileName string
		want     string
	}{
		{fileName: "image.PNG", want: "image/png"},
		{fileName: "doc.PDF", want: "application/pdf"},
		{fileName: "archive.tar.gz", want: "application/gzip"},
		{fileName: "unknown.xyz", want: DefaultContentType},
		{fileName: "noextension", want: DefaultContentType},
	}
	for _, tt := range tests {
		if got := ResolveContentType(tt.fileName); got != tt.want {
			t.Errorf("ResolveContentType(%q) = %q, want %q", tt.fileName, got, tt.want)
		}
	}
}

func TestRetryHonorsRetryAfterAndBackoff(t *testing.T) {
	previousBase := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = previousBase })

	fake := newFakeS3(t)
	storage := fake.newStorage(t)

	// Three transient 503s then success: the outer loop must retry to the
	// end and the object must be stored once.
	fake.mu.Lock()
	fake.putFailures = 3
	fake.mu.Unlock()

	if err := storage.UploadText(context.Background(), "metadata.json", "{}"); err != nil {
		t.Fatalf("UploadText should succeed after retries: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.putAttempts != 4 {
		t.Errorf("put attempts = %d, want 4 (3 failures + 1 success)", fake.putAttempts)
	}
	if len(fake.puts) != 1 {
		t.Errorf("recorded puts = %d, want 1", len(fake.puts))
	}
}

// errorAfterPayloadReader yields its payload once, then fails every further
// read with err — the shape of a size-capped attachment stream that hits its
// limit mid-body.
type errorAfterPayloadReader struct {
	payload []byte
	err     error
	done    bool
}

func (r *errorAfterPayloadReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.payload), nil
	}
	return 0, r.err
}

func TestProducePartsFailsOnMidStreamReadError(t *testing.T) {
	readErr := errors.New("attachment exceeds the 100 byte limit")
	jobs := make(chan multipartJob, multipartParallelParts-1)
	state := newMultipartState()

	err := produceParts(context.Background(), &errorAfterPayloadReader{payload: []byte("abc"), err: readErr}, jobs, state)
	if !errors.Is(err, readErr) {
		t.Fatalf("err = %v, want %v", err, readErr)
	}
	if state.err() != nil {
		t.Errorf("worker error = %v, want none", state.err())
	}
}

func TestProducePartsKeepsShortFinalPartOnUnexpectedEOF(t *testing.T) {
	jobs := make(chan multipartJob, multipartParallelParts-1)
	state := newMultipartState()

	err := produceParts(context.Background(), bytes.NewReader([]byte("abc")), jobs, state)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	job := <-jobs
	if string(job.data) != "abc" {
		t.Errorf("final part = %q, want %q", job.data, "abc")
	}
}
