package store

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/neurekadev/git-backup/internal/paths"
)

// ListObjectKeys returns every object key under the prefix, following
// pagination to the end of the listing.
func (s *ObjectStorage) ListObjectKeys(ctx context.Context, prefix string) ([]string, error) {
	normalizedPrefix := paths.EnsurePrefix(prefix)
	slog.Debug("Listing object keys.", "prefix", normalizedPrefix)

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(normalizedPrefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, s.reportFailure("list objects", normalizedPrefix, err)
		}
		for _, object := range page.Contents {
			if object.Key != nil && strings.TrimSpace(*object.Key) != "" {
				keys = append(keys, *object.Key)
			}
		}
	}

	slog.Debug("Object key listing completed.", "prefix", normalizedPrefix, "keyCount", len(keys))
	return keys, nil
}

// DeleteObjects removes the given keys in batches of 1000. Some S3-compatible
// providers reject the bulk-delete request shape; the first such response
// permanently switches this call chain to single-object deletes.
func (s *ObjectStorage) DeleteObjects(ctx context.Context, objectKeys []string) error {
	seen := make(map[string]struct{}, len(objectKeys))
	var keys []string
	for _, key := range objectKeys {
		trimmed := strings.Trim(strings.TrimSpace(key), "/")
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		keys = append(keys, trimmed)
	}

	if len(keys) == 0 {
		slog.Debug("No object deletions needed.")
		return nil
	}

	slog.Debug("Deleting objects.", "count", len(keys))
	useSingleObjectDeletes := false

	for start := 0; start < len(keys); start += deleteBatchSize {
		end := min(start+deleteBatchSize, len(keys))
		batch := keys[start:end]

		if useSingleObjectDeletes {
			if err := s.deleteObjectsIndividually(ctx, batch); err != nil {
				return err
			}
			continue
		}

		slog.Debug("Deleting object batch.", "batchSize", len(batch))
		err := s.execute(ctx, "delete objects", strconv.Itoa(len(keys)), func(ctx context.Context) error {
			_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(s.bucket),
				Delete: &types.Delete{Objects: keysToIdentifiers(batch), Quiet: aws.Bool(true)},
			})
			return err
		})
		if err == nil {
			continue
		}
		if !isBulkDeleteSchemaError(err) {
			return err
		}

		slog.Warn("Bulk delete was rejected by the storage provider. Switching to single-object deletes.",
			"batchSize", len(batch), "error", err.Error())
		useSingleObjectDeletes = true
		if err := s.deleteObjectsIndividually(ctx, batch); err != nil {
			return err
		}
	}

	return nil
}

func (s *ObjectStorage) deleteObjectsIndividually(ctx context.Context, batch []string) error {
	deletedCount := 0
	for _, key := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.execute(ctx, "delete object", key, func(ctx context.Context) error {
			_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
			})
			return err
		}); err != nil {
			return err
		}
		deletedCount++
	}

	slog.Debug("Single-object delete batch completed.", "batchSize", deletedCount)
	return nil
}

func keysToIdentifiers(keys []string) []types.ObjectIdentifier {
	identifiers := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		identifiers = append(identifiers, types.ObjectIdentifier{Key: aws.String(key)})
	}
	return identifiers
}
