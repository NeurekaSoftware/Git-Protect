// Package store is the daemon's S3-compatible object storage layer.
//
// Snapshots and metadata are streamed with bounded memory: archives are
// produced on the fly (tar → gzip → multipart, no temp file on disk) and
// uploaded as 16 MiB parts with two workers. Wire compatibility with strict
// S3 providers is preserved: multipart uploads carry no Content-Type and no
// checksum trailers.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/neurekadev/git-backup/internal/config"
)

const (
	jsonContentType = "application/json"

	// MultipartPartSizeBytes is the size of one uploaded part.
	MultipartPartSizeBytes = 16 * 1024 * 1024

	// multipartParallelParts bounds how many parts upload concurrently. Peak
	// upload RAM is partSize x (parallelism + 1) — 2 holds ~48 MiB in flight.
	multipartParallelParts = 2

	// SinglePutMaxBytes is the largest payload sent as a single PutObject.
	// Below this the whole body is held in memory briefly, which is cheaper
	// than a multipart round trip; above it, streaming multipart keeps peak
	// memory flat.
	SinglePutMaxBytes = 5 * 1024 * 1024

	// perRequestTimeout caps a single S3 request attempt.
	perRequestTimeout = 15 * time.Minute

	// maxAttempts bounds the durable outer retry loop around transient
	// provider errors.
	maxAttempts = 5

	// retryBackoffBaseDefault is the exponential backoff unit's default; the
	// package-level retryBackoffBase is a variable so tests can shorten waits.
	retryBackoffBaseDefault = 1 * time.Second

	// maxRetryAfterCap caps the Retry-After honor for 5xx responses; 429
	// responses are honored uncapped.
	maxRetryAfterCap = 30 * time.Second
)

// retryBackoffBase is the exponential backoff unit for the outer retry loop.
var retryBackoffBase = retryBackoffBaseDefault

// ObjectStorage writes snapshots and metadata documents to one bucket on an
// S3-compatible endpoint.
type ObjectStorage struct {
	bucket         string
	accessKeyID    string
	secretKey      string
	client         *s3.Client
	endpoint       string
	resolved       string
	forcePathStyle bool
	signature      string
}

// NewObjectStorage builds the storage client from validated settings.
func NewObjectStorage(storage config.Storage) (*ObjectStorage, error) {
	require := func(value, name string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("storage configuration '%s' is required", name)
		}
		return value, nil
	}

	bucket, err := require(storage.Bucket, "storage.bucket")
	if err != nil {
		return nil, err
	}
	endpoint, err := require(storage.Endpoint, "storage.endpoint")
	if err != nil {
		return nil, err
	}
	region, err := require(storage.Region, "storage.region")
	if err != nil {
		return nil, err
	}
	accessKeyID, err := require(storage.AccessKeyID, "storage.accessKeyId")
	if err != nil {
		return nil, err
	}
	secretKey, err := require(storage.SecretAccessKey, "storage.secretAccessKey")
	if err != nil {
		return nil, err
	}

	resolved := resolveEndpoint(endpoint, bucket, storage.ForcePathStyle)
	sdkEndpoint, usePathStyle := sdkEndpointFor(endpoint, bucket, storage.ForcePathStyle)

	awsConfig := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretKey, ""),
		HTTPClient:  &http.Client{Timeout: perRequestTimeout},
	}
	// The SDK client makes one attempt; the durable outer loop in execute owns
	// transient-error retries with the documented backoff caps.
	awsConfig.Retryer = func() aws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 1) }
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(sdkEndpoint)
		options.UsePathStyle = usePathStyle
	})

	slog.Debug("Object storage client initialized.", "provider", "GenericS3")
	slog.Debug("Object storage settings.",
		"endpoint", endpoint,
		"resolvedEndpoint", resolved,
		"region", region,
		"bucket", bucket,
		"forcePathStyle", storage.ForcePathStyle,
		"payloadSignatureMode", storage.PayloadSignatureMode)

	return &ObjectStorage{
		bucket:         bucket,
		accessKeyID:    accessKeyID,
		secretKey:      secretKey,
		client:         client,
		endpoint:       endpoint,
		resolved:       resolved,
		forcePathStyle: storage.ForcePathStyle,
		signature:      storage.PayloadSignatureMode,
	}, nil
}

// resolveEndpoint ports the endpoint template resolution: a user-provided
// template is taken literally, otherwise virtual-host style derives
// scheme://{Bucket}.host[:port][/path], with an already bucket-prefixed host
// stripped so the bucket is not doubled. Path style keeps the endpoint as-is.
func resolveEndpoint(configuredEndpoint, bucket string, forcePathStyle bool) string {
	endpoint := strings.TrimSpace(configuredEndpoint)
	trimmed := strings.TrimSuffix(endpoint, "/")

	if strings.Contains(endpoint, "{") {
		return trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || forcePathStyle {
		return trimmed
	}

	host := parsed.Hostname()
	bucketPrefix := bucket + "."
	if strings.HasPrefix(strings.ToLower(host), strings.ToLower(bucketPrefix)) {
		host = host[len(bucketPrefix):]
	}

	authority := host
	if parsed.Port() != "" {
		authority = host + ":" + parsed.Port()
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	return fmt.Sprintf("%s://{Bucket}.%s%s", parsed.Scheme, authority, path)
}

// sdkEndpointFor translates the resolved template into the AWS SDK's
// BaseEndpoint form plus its addressing style. The substituted or stripped
// endpoint reaches the same wire URLs the previous client produced.
func sdkEndpointFor(configuredEndpoint, bucket string, forcePathStyle bool) (string, bool) {
	template := resolveEndpoint(configuredEndpoint, bucket, forcePathStyle)

	switch {
	case strings.Contains(configuredEndpoint, "{"):
		// The template already states where the bucket lives.
		return strings.ReplaceAll(template, "{Bucket}", bucket), true
	case forcePathStyle:
		return template, true
	default:
		return strings.ReplaceAll(template, "{Bucket}.", ""), false
	}
}

// execute runs one S3 operation under the durable retry policy and the single
// uniform failure model: genuine failures are logged with their context and
// returned as a domain error. Bulk-delete schema errors are returned untouched
// for the caller's per-object fallback.
func (s *ObjectStorage) execute(ctx context.Context, operation, objectKey string, action func(context.Context) error) error {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := action(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		if isBulkDeleteSchemaError(err) {
			return err
		}

		status, retryAfter, ok := httpStatusOf(err)
		if ok {
			if !isRetryableStatus(status) || attempt == maxAttempts {
				break
			}
		} else if !isTransportError(err) || attempt == maxAttempts {
			// No HTTP response: connection-level failures (timeouts, resets)
			// are retried; anything else fails fast.
			break
		}

		delay := backoffDelay(attempt, retryAfter, status)
		slog.Debug("Retrying object storage operation after transient error.",
			"operation", operation, "status", status, "attempt", attempt, "delayMs", delay.Milliseconds())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return s.reportFailure(operation, objectKey, lastErr)
}

// reportFailure is the single translation point for storage failures: log with
// context, then surface a domain error.
func (s *ObjectStorage) reportFailure(operation, objectKey string, err error) error {
	detail := describeError(err)
	slog.Error("Object storage operation failed.",
		"operation", operation, "objectKey", objectKey, "detail", detail)
	return fmt.Errorf("storage: failed to %s '%s'. %s", operation, objectKey, detail)
}

// httpStatusOf extracts the HTTP status and Retry-After hint from an SDK
// error, reporting false when the error carries no HTTP response.
func httpStatusOf(err error) (int, time.Duration, bool) {
	var responseError *awshttp.ResponseError
	if !errors.As(err, &responseError) {
		return 0, 0, false
	}

	status := responseError.HTTPStatusCode()
	var retryAfter time.Duration
	if responseError.Response != nil {
		retryAfter = parseRetryAfter(responseError.Response.Header.Get("Retry-After"))
	}
	return status, retryAfter, true
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// isTransportError reports whether err is a connection-level failure (timeout,
// reset, refused) that a retry can plausibly fix.
func isTransportError(err error) bool {
	var transportErr *url.Error
	if errors.As(err, &transportErr) {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	return false
}

// backoffDelay returns the wait before the next attempt: an exponential
// component with jitter, or the server's Retry-After when present (honored
// uncapped for 429, capped for 5xx).
func backoffDelay(attempt int, retryAfter time.Duration, status int) time.Duration {
	if retryAfter > 0 {
		if status == http.StatusTooManyRequests {
			return retryAfter
		}
		return min(retryAfter, maxRetryAfterCap)
	}

	delay := retryBackoffBase << (attempt - 1)
	jitter := time.Duration(rand.Int63n(int64(time.Second)))
	return delay + jitter
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		return max(0, time.Until(when))
	}
	return 0
}

// describeError renders the most specific detail an SDK error offers: the S3
// error code and message when present, otherwise the status code.
func describeError(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() != "" {
		message := apiErr.ErrorMessage()
		if strings.TrimSpace(message) == "" {
			return apiErr.ErrorCode()
		}
		return fmt.Sprintf("%s: %s", apiErr.ErrorCode(), message)
	}

	if status, _, ok := httpStatusOf(err); ok {
		return fmt.Sprintf("statusCode=%d", status)
	}
	return strings.TrimSpace(err.Error())
}

// isBulkDeleteSchemaError recognizes the responses some S3-compatible
// providers return when their bulk-delete implementation rejects the request
// shape, so the caller can fall back to single-object deletes.
func isBulkDeleteSchemaError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "MalformedXML") ||
		strings.Contains(message, "XML you provided was not well formed") ||
		strings.Contains(message, "did not validate against our published schema")
}

// normalizeObjectKey trims the surrounding slashes an object key must not
// carry and rejects an empty one. Shared by every upload path so the bucket's
// only writers cannot disagree about what a valid key is.
func normalizeObjectKey(objectKey string) (string, error) {
	normalized := strings.Trim(objectKey, "/")
	if strings.TrimSpace(normalized) == "" {
		return "", errors.New("object key is required")
	}
	return normalized, nil
}
