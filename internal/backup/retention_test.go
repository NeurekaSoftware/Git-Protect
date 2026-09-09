package backup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neurekadev/git-backup/internal/config"
)

func TestRetentionProtectsNewestAndDeletesExpired(t *testing.T) {
	storage := newFakeStorage()
	now := time.Now().UTC()
	prefix := "repositories/provider/github/octo/repo"

	// Five snapshots: 40, 30, 20, 10, and 5 days old. With a 15-day window and
	// a minimum of 2, the two newest survive regardless of age; the 40d, 30d,
	// and 20d snapshots are all older than the cutoff and expire.
	for _, ageDays := range []int{40, 30, 20, 10, 5} {
		timestamp := now.AddDate(0, 0, -ageDays).Unix()
		storage.objects[fmt.Sprintf("%s/%d_repo.tar.gz", prefix, timestamp)] = "tar"
	}
	storage.objects[prefix+"/metadata.json"] = "meta"

	retention := NewRetentionService(func(*config.Settings) (ObjectStorage, error) { return storage, nil })
	settings := testSettings()
	settings.Storage.Retention = 15
	settings.Storage.RetentionMinimum = 2

	if err := retention.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if len(storage.deleted) != 1 {
		t.Fatalf("delete calls = %d, want 1 batch", len(storage.deleted))
	}
	if len(storage.deleted[0]) != 3 {
		t.Fatalf("deleted keys = %v, want the three oldest snapshots", storage.deleted[0])
	}
	for _, ageDays := range []int{40, 30, 20} {
		expired := now.AddDate(0, 0, -ageDays).Unix()
		want := fmt.Sprintf("%s/%d_repo.tar.gz", prefix, expired)
		found := false
		for _, key := range storage.deleted[0] {
			if key == want {
				found = true
			}
		}
		if !found {
			t.Errorf("snapshot %q should be deleted", want)
		}
	}
	if !storage.has(fmt.Sprintf("%s/%d_repo.tar.gz", prefix, now.AddDate(0, 0, -5).Unix())) {
		t.Error("the newest snapshot must survive")
	}
	if !storage.has(fmt.Sprintf("%s/%d_repo.tar.gz", prefix, now.AddDate(0, 0, -10).Unix())) {
		t.Error("the second-newest snapshot is protected by retentionMinimum")
	}
}

func TestRetentionDisabledWhenZeroOrNegative(t *testing.T) {
	storage := newFakeStorage()
	storage.objects["repositories/provider/github/octo/repo/1_repo.tar.gz"] = "tar"

	retention := NewRetentionService(func(*config.Settings) (ObjectStorage, error) { return storage, nil })
	settings := testSettings()
	settings.Storage.Retention = 0

	if err := retention.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if len(storage.deleted) != 0 {
		t.Fatalf("deletes = %v, want none while retention is disabled", storage.deleted)
	}

	settings.Storage.Retention = -3
	if err := retention.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if len(storage.deleted) != 0 {
		t.Fatalf("deletes = %v, want none while retention is disabled", storage.deleted)
	}
}

func TestRetentionReclaimsOrphansOfEmptiedRepositories(t *testing.T) {
	storage := newFakeStorage()
	now := time.Now().UTC()

	expiredPrefix := "repositories/provider/github/octo/expired"
	storage.objects[fmt.Sprintf("%s/%d_repo.tar.gz", expiredPrefix, now.AddDate(0, 0, -60).Unix())] = "tar"
	storage.objects[expiredPrefix+"/metadata.json"] = "meta"
	storage.objects[expiredPrefix+"/issues/7.json"] = "doc"
	storage.objects[expiredPrefix+"/issues/attachments/7/file.png"] = "attachment"
	storage.objects[expiredPrefix+"/releases/v1.json"] = "doc"

	// A project snippet nested under the expired repository is an independent
	// repository with its own snapshots: it must be left untouched.
	nestedSnippet := expiredPrefix + "/snippets/42"
	storage.objects[fmt.Sprintf("%s/%d_repo.tar.gz", nestedSnippet, now.AddDate(0, 0, -1).Unix())] = "tar"

	// A healthy repository is untouched.
	healthyPrefix := "repositories/provider/github/octo/healthy"
	storage.objects[fmt.Sprintf("%s/%d_repo.tar.gz", healthyPrefix, now.AddDate(0, 0, -1).Unix())] = "tar"

	retention := NewRetentionService(func(*config.Settings) (ObjectStorage, error) { return storage, nil })
	settings := testSettings()
	settings.Storage.Retention = 30
	// A minimum of 0 lets the last snapshot of an abandoned repository expire,
	// which is what produces the emptied prefixes this test exercises.
	settings.Storage.RetentionMinimum = 0

	if err := retention.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if storage.has(expiredPrefix + "/metadata.json") {
		t.Error("the emptied repository's metadata.json should be reclaimed")
	}
	if storage.has(expiredPrefix + "/issues/7.json") {
		t.Error("the emptied repository's issue documents should be reclaimed")
	}
	if storage.has(expiredPrefix + "/issues/attachments/7/file.png") {
		t.Error("the emptied repository's attachments should be reclaimed")
	}
	if storage.has(expiredPrefix + "/releases/v1.json") {
		t.Error("the emptied repository's release documents should be reclaimed")
	}
	if !storage.has(fmt.Sprintf("%s/%d_repo.tar.gz", nestedSnippet, now.AddDate(0, 0, -1).Unix())) {
		t.Error("the nested snippet is its own repository and must be untouched")
	}
	if !storage.has(fmt.Sprintf("%s/%d_repo.tar.gz", healthyPrefix, now.AddDate(0, 0, -1).Unix())) {
		t.Error("the healthy repository must be untouched")
	}
}

func TestRetentionCoversBothRoots(t *testing.T) {
	storage := newFakeStorage()
	now := time.Now().UTC()

	// A gist under the snippets root with only old snapshots empties too.
	gistPrefix := "snippets/provider/github/abc"
	storage.objects[fmt.Sprintf("%s/%d_repo.tar.gz", gistPrefix, now.AddDate(0, 0, -90).Unix())] = "tar"
	storage.objects[gistPrefix+"/metadata.json"] = "meta"

	retention := NewRetentionService(func(*config.Settings) (ObjectStorage, error) { return storage, nil })
	settings := testSettings()
	settings.Storage.Retention = 30
	settings.Storage.RetentionMinimum = 0

	if err := retention.Run(context.Background(), settings); err != nil {
		t.Fatal(err)
	}

	if storage.has(gistPrefix + "/metadata.json") {
		t.Error("snippets-root repositories should be retained and reclaimed like any other")
	}
}
