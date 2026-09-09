package forge

// JSON mappers for the Gitea-lineage API shape, shared by GitHub and Forgejo.
// They deliberately stay off GitLab, whose field names differ entirely: a
// GitLab payload has no "number", so the issue mapper would return nothing and
// silently drop the item rather than fail.

// stringPtr yields nil for a missing value so optional fields serialize as
// null, matching the stored-document format.
func stringPtr(value string, ok bool) *string {
	if !ok {
		return nil
	}
	return &value
}

func boolPtr(value bool) *bool { return &value }

func mapGiteaRepository(item map[string]any, isStarred bool) (DiscoveredRepository, bool) {
	cloneURL, ok := jsonString(item, "clone_url")
	if !ok || cloneURL == "" {
		return DiscoveredRepository{}, false
	}

	webURL, _ := jsonString(item, "html_url")
	return DiscoveredRepository{
		CloneURL:  cloneURL,
		WebURL:    webURL,
		IsStarred: isStarred,
	}, true
}

// mapGiteaComment maps a Gitea-lineage comment (GitHub, Forgejo): author at
// user.login.
func mapGiteaComment(item map[string]any) (Comment, bool) {
	return mapComment(item, "user", "login", false)
}

// mapComment maps a comment, reading the author from
// authorObject.authorProperty. Providers differ only in that path (GitHub and
// Forgejo user.login, GitLab author.username) and whether a system flag
// distinguishes generated notes.
func mapComment(item map[string]any, authorObject, authorProperty string, readSystemFlag bool) (Comment, bool) {
	body, hasBody := jsonString(item, "body")
	author, hasAuthor := jsonNestedString(item, authorObject, authorProperty)
	if !hasBody && !hasAuthor {
		return Comment{}, false
	}

	id, hasID := jsonInt64(item, "id")
	createdAt, _ := jsonTime(item, "created_at")
	updatedAt, _ := jsonTime(item, "updated_at")
	return Comment{
		ID:        boolPtrInt64(hasID, id),
		Author:    stringPtr(author, hasAuthor),
		Body:      stringPtr(body, hasBody),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		System:    readSystemFlag && jsonBool(item, "system"),
	}, true
}

func boolPtrInt64(set bool, value int64) *int64 {
	if !set {
		return nil
	}
	return &value
}

// mapGiteaIssue maps the shared issue fields. Attachments are populated by the
// caller — GitHub scans the body, Forgejo reads the assets array — so this
// leaves them empty.
func mapGiteaIssue(item map[string]any) (*Issue, bool) {
	number, hasNumber := jsonInt64(item, "number")
	title, _ := jsonString(item, "title")
	if !hasNumber || title == "" {
		return nil, false
	}

	author, hasAuthor := jsonNestedString(item, "user", "login")
	createdAt, _ := jsonTime(item, "created_at")
	updatedAt, _ := jsonTime(item, "updated_at")
	closedAt, _ := jsonTime(item, "closed_at")
	webURL, _ := jsonString(item, "html_url")
	state, hasState := jsonString(item, "state")
	body, hasBody := jsonString(item, "body")
	return &Issue{
		Number:    number,
		Title:     title,
		State:     stringPtr(state, hasState),
		Author:    stringPtr(author, hasAuthor),
		Body:      stringPtr(body, hasBody),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
		ClosedAt:  closedAt,
		Labels:    jsonLabels(item, "labels"),
		WebURL:    stringPtr(webURL, webURL != ""),
	}, true
}

// mapGiteaPullRequest maps the shared pull-request fields; attachments are
// populated by the caller.
func mapGiteaPullRequest(item map[string]any) (*MergeRequest, bool) {
	number, hasNumber := jsonInt64(item, "number")
	title, _ := jsonString(item, "title")
	if !hasNumber || title == "" {
		return nil, false
	}

	author, hasAuthor := jsonNestedString(item, "user", "login")
	sourceBranch, hasSource := jsonNestedString(item, "head", "ref")
	targetBranch, hasTarget := jsonNestedString(item, "base", "ref")
	createdAt, _ := jsonTime(item, "created_at")
	updatedAt, _ := jsonTime(item, "updated_at")
	mergedAt, _ := jsonTime(item, "merged_at")
	closedAt, _ := jsonTime(item, "closed_at")
	webURL, _ := jsonString(item, "html_url")
	state, hasState := jsonString(item, "state")
	body, hasBody := jsonString(item, "body")
	return &MergeRequest{
		Number:       number,
		Title:        title,
		State:        stringPtr(state, hasState),
		Author:       stringPtr(author, hasAuthor),
		Body:         stringPtr(body, hasBody),
		SourceBranch: stringPtr(sourceBranch, hasSource),
		TargetBranch: stringPtr(targetBranch, hasTarget),
		CreatedAt:    createdAt,
		UpdatedAt:    updatedAt,
		MergedAt:     mergedAt,
		ClosedAt:     closedAt,
		Labels:       jsonLabels(item, "labels"),
		WebURL:       stringPtr(webURL, webURL != ""),
	}, true
}

// mapGiteaRelease maps a release, including its downloadable assets from the
// assets array. Draft and prerelease are always materialized (false when the
// fields are absent), mirroring the original mapper.
func mapGiteaRelease(item map[string]any) (*Release, bool) {
	tag, _ := jsonString(item, "tag_name")
	if tag == "" {
		return nil, false
	}

	name, hasName := jsonString(item, "name")
	author, hasAuthor := jsonNestedString(item, "author", "login")
	createdAt, _ := jsonTime(item, "created_at")
	publishedAt, _ := jsonTime(item, "published_at")
	webURL, _ := jsonString(item, "html_url")
	body, hasBody := jsonString(item, "body")
	return &Release{
		Tag:         tag,
		Name:        stringPtr(name, hasName),
		Body:        stringPtr(body, hasBody),
		Author:      stringPtr(author, hasAuthor),
		Draft:       boolPtr(jsonBool(item, "draft")),
		Prerelease:  boolPtr(jsonBool(item, "prerelease")),
		CreatedAt:   createdAt,
		PublishedAt: publishedAt,
		WebURL:      stringPtr(webURL, webURL != ""),
		Attachments: extractAssetArray(item),
	}, true
}

// extractAssetArray extracts downloadable assets from a Gitea-lineage assets
// array, deduping by download URL so a release (or issue/MR) never yields the
// same file twice.
func extractAssetArray(item map[string]any) []Attachment {
	assets, ok := item["assets"].([]any)
	if !ok {
		return nil
	}

	var attachments []Attachment
	seen := make(map[string]struct{}, len(assets))
	for _, element := range assets {
		asset, ok := element.(map[string]any)
		if !ok {
			continue
		}
		assetURL, _ := jsonString(asset, "browser_download_url")
		name, _ := jsonString(asset, "name")
		if assetURL == "" || name == "" {
			continue
		}
		if _, duplicate := seen[assetURL]; duplicate {
			continue
		}
		seen[assetURL] = struct{}{}

		attachment := Attachment{
			FileName:     BuildStorageFileName(assetURL, name),
			OriginalPath: assetURL,
			DownloadURL:  assetURL,
		}
		if size, ok := jsonInt64(asset, "size"); ok {
			attachment.SizeBytes = &size
		}
		attachments = append(attachments, attachment)
	}
	return attachments
}

// mergeAttachments concatenates two attachment lists, deduping by
// OriginalPath with first-wins semantics.
func mergeAttachments(first, second []Attachment) []Attachment {
	return distinctByKey(append(append([]Attachment{}, first...), second...), func(a Attachment) string {
		return a.OriginalPath
	})
}
