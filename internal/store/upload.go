package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// UploadDirectoryAsTarGz streams the directory at localDirectory as a gzipped
// tar archive into objectKey.
//
// The archive is produced on the fly through a bounded pipe — no temp .tar.gz
// lands on disk, and peak RAM is just the in-flight part buffers. Entries are
// written relative to the directory (no base-directory prefix) and compression
// is fastest: a bare git mirror is almost entirely packfile data that git has
// already zlib-compressed, so max-effort deflate spends far more CPU for
// negligible extra reduction.
func (s *ObjectStorage) UploadDirectoryAsTarGz(ctx context.Context, localDirectory, objectKey string) error {
	info, err := os.Stat(localDirectory)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("directory '%s' does not exist", localDirectory)
	}

	key, err := normalizeObjectKey(objectKey)
	if err != nil {
		return err
	}

	slog.Debug("Streaming archive upload.", "localDirectory", localDirectory, "objectKey", key)

	pipeReader, pipeWriter := io.Pipe()
	produceDone := make(chan error, 1)
	go func() {
		produceDone <- writeTarGz(localDirectory, pipeWriter)
	}()

	// Deliberately no Content-Type on the multipart path: the initiate request
	// (POST ?uploads) has no body, and a strict S3 provider (e.g. Backblaze B2)
	// rejects the request when a Content-Type is signed but not present. The
	// stored object gets the provider's default type.
	uploadErr := s.multipartUpload(ctx, key, pipeReader)
	_ = pipeReader.Close()

	if uploadErr != nil {
		// Unblock the producer; the upload failure is the primary error and a
		// producer failure is secondary.
		_ = pipeReader.CloseWithError(uploadErr)
		if produceErr := <-produceDone; produceErr != nil {
			slog.Debug("Archive producer stopped after upload failure.", "error", produceErr.Error())
		}
		return uploadErr
	}

	if produceErr := <-produceDone; produceErr != nil {
		return produceErr
	}

	slog.Info("Archive uploaded.", "objectKey", key)
	return nil
}

// writeTarGz writes the directory's contents into w as a gzipped tar,
// relative to the directory root.
func writeTarGz(localDirectory string, w *io.PipeWriter) error {
	gzipWriter := gzip.NewWriter(w)
	tarWriter := tar.NewWriter(gzipWriter)

	err := appendDirectory(tarWriter, localDirectory)
	closeErr := tarWriter.Close()
	gzipErr := gzipWriter.Close()

	// A producer failure must reach the upload side as a stream error, not a
	// clean EOF, so the multipart upload fails instead of storing a truncated
	// archive.
	if err != nil {
		_ = w.CloseWithError(err)
		return err
	}
	if closeErr != nil {
		_ = w.CloseWithError(closeErr)
		return closeErr
	}
	if gzipErr != nil {
		_ = w.CloseWithError(gzipErr)
		return gzipErr
	}
	return w.Close()
}

// appendDirectory emits one tar entry per directory, file, and symlink under
// localDirectory, with paths relative to it and slash-separated so archives
// built on any host look the same.
func appendDirectory(tarWriter *tar.Writer, localDirectory string) error {
	return filepath.WalkDir(localDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(localDirectory, path)
		if err != nil {
			return err
		}
		if relative == "." {
			// The archive has no base directory; children only.
			return nil
		}
		name := filepath.ToSlash(relative)

		info, err := entry.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name

		switch {
		case entry.IsDir():
			header.Name = name + "/"
			return tarWriter.WriteHeader(header)

		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = link
			header.Size = 0
			return tarWriter.WriteHeader(header)

		case info.Mode().IsRegular():
			if err := tarWriter.WriteHeader(header); err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tarWriter, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr

		default:
			// A bare git mirror contains nothing else.
			return nil
		}
	})
}

// UploadText stores a text document gzip-compressed with
// Content-Encoding: gzip. Documents and manifests are repetitive text that
// gzips several times over, cutting upload egress and stored size; a client
// that honors Content-Encoding still reads plain JSON, and the object key
// stays *.json.
func (s *ObjectStorage) UploadText(ctx context.Context, objectKey, content string) error {
	key, err := normalizeObjectKey(objectKey)
	if err != nil {
		return err
	}

	slog.Debug("Uploading object.", "objectKey", key, "contentType", jsonContentType, "contentEncoding", "gzip")

	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write([]byte(content)); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}

	return s.execute(ctx, "upload object", key, func(ctx context.Context) error {
		_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:          aws.String(s.bucket),
			Key:             aws.String(key),
			Body:            bytes.NewReader(compressed.Bytes()),
			ContentType:     aws.String(jsonContentType),
			ContentEncoding: aws.String("gzip"),
		})
		return err
	})
}

// UploadStream stores an attachment stream. A small payload with a declared
// length goes out as a single PutObject carrying its Content-Type; a large or
// unknown-size payload streams via multipart (no Content-Type, for the same
// strict-provider reason as the archive path), never buffered fully in memory.
// A negative knownLength means the length is unknown.
func (s *ObjectStorage) UploadStream(ctx context.Context, objectKey string, content io.Reader, contentType string, knownLength int64) error {
	key, err := normalizeObjectKey(objectKey)
	if err != nil {
		return err
	}

	resolvedContentType := contentType
	if resolvedContentType == "" {
		resolvedContentType = DefaultContentType
	}

	if knownLength >= 0 && knownLength <= SinglePutMaxBytes {
		slog.Debug("Uploading object.", "objectKey", key, "contentType", resolvedContentType, "bytes", knownLength)

		buffer := make([]byte, knownLength)
		if _, err := io.ReadFull(content, buffer); err != nil {
			return fmt.Errorf("read attachment: %w", err)
		}

		return s.execute(ctx, "upload object", key, func(ctx context.Context) error {
			_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket:      aws.String(s.bucket),
				Key:         aws.String(key),
				Body:        bytes.NewReader(buffer),
				ContentType: aws.String(resolvedContentType),
			})
			return err
		})
	}

	slog.Debug("Streaming object upload.", "objectKey", key)
	return s.multipartUpload(ctx, key, content)
}

// multipartJob is one buffered part awaiting upload.
type multipartJob struct {
	number int32
	data   []byte
}

// multipartState collects completed parts and the first failure.
type multipartState struct {
	mu       sync.Mutex
	results  []types.CompletedPart
	firstErr error
	done     chan struct{}
}

func newMultipartState() *multipartState {
	return &multipartState{done: make(chan struct{})}
}

func (m *multipartState) add(part types.CompletedPart) {
	m.mu.Lock()
	m.results = append(m.results, part)
	m.mu.Unlock()
}

func (m *multipartState) fail(err error) {
	m.mu.Lock()
	if m.firstErr == nil {
		m.firstErr = err
		close(m.done)
	}
	m.mu.Unlock()
}

func (m *multipartState) err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstErr
}

func (m *multipartState) parts() []types.CompletedPart {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.Slice(m.results, func(i, j int) bool { return *m.results[i].PartNumber < *m.results[j].PartNumber })
	return m.results
}

// bufferPool recycles the fixed-size part buffers so a long archive upload
// does not churn the heap.
var bufferPool = sync.Pool{
	New: func() any { buffer := make([]byte, MultipartPartSizeBytes); return &buffer },
}

// multipartUpload uploads a stream with the S3 multipart protocol: 16 MiB
// parts, two concurrent part uploads, and a bounded read-ahead so peak memory
// stays at partSize x (parallelism + 1). The stream's length need not be
// known, and no Content-Type is ever attached to the initiate request.
func (s *ObjectStorage) multipartUpload(ctx context.Context, key string, body io.Reader) error {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return s.reportFailure("initiate multipart upload", key, err)
	}
	uploadID := created.UploadId

	state := newMultipartState()
	jobs := make(chan multipartJob, multipartParallelParts-1)

	var workers sync.WaitGroup
	for worker := 0; worker < multipartParallelParts; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				uploaded, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
					Bucket:     aws.String(s.bucket),
					Key:        aws.String(key),
					UploadId:   uploadID,
					PartNumber: aws.Int32(job.number),
					Body:       bytes.NewReader(job.data),
				})
				// job.data may be the short final part; restore the pooled
				// buffer's full length before returning it.
				full := job.data[:cap(job.data)]
				bufferPool.Put(&full)
				if err != nil {
					state.fail(err)
					return
				}
				state.add(types.CompletedPart{PartNumber: aws.Int32(job.number), ETag: uploaded.ETag})
			}
		}()
	}

	produceErr := produceParts(ctx, body, jobs, state)
	close(jobs)
	workers.Wait()

	if err := state.err(); err != nil {
		s.abortMultipart(ctx, key, uploadID)
		return s.reportFailure("upload part", key, err)
	}
	if produceErr != nil {
		s.abortMultipart(ctx, key, uploadID)
		if errors.Is(produceErr, context.Canceled) || errors.Is(produceErr, context.DeadlineExceeded) {
			return produceErr
		}
		return s.reportFailure("read archive stream", key, produceErr)
	}

	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: state.parts(),
		},
	})
	if err != nil {
		s.abortMultipart(ctx, key, uploadID)
		return s.reportFailure("complete multipart upload", key, err)
	}
	return nil
}

// produceParts reads the stream into part-sized buffers and feeds the bounded
// job channel, stopping at end of stream, failure, or cancellation. It closes
// nothing; the caller owns the channel.
func produceParts(ctx context.Context, body io.Reader, jobs chan<- multipartJob, state *multipartState) error {
	for number := int32(1); ; number++ {
		buffer := *(bufferPool.Get().(*[]byte))

		n, readErr := io.ReadFull(body, buffer)
		if n == 0 {
			bufferPool.Put(&buffer)
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr == nil {
				continue
			}
			return fmt.Errorf("read stream: %w", readErr)
		}

		job := multipartJob{number: number, data: buffer[:n]}
		select {
		case jobs <- job:
		case <-state.done:
			bufferPool.Put(&buffer)
			return state.err()
		case <-ctx.Done():
			bufferPool.Put(&buffer)
			return ctx.Err()
		}

		if readErr != nil {
			// io.ErrUnexpectedEOF: the final, short part is queued.
			return nil
		}
	}
}

func (s *ObjectStorage) abortMultipart(ctx context.Context, key string, uploadID *string) {
	if uploadID == nil {
		return
	}
	// The caller's context may already be cancelled; the abort must still run.
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if _, err := s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: uploadID,
	}); err != nil {
		slog.Debug("Failed to abort multipart upload.", "objectKey", key, "error", err.Error())
	}
}
