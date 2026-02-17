# S3 Cost Optimization Guardrails

## Objective
- Minimize S3 Class A operations during normal backup and mirror runs.
- Prefer archive-level hash comparisons and index-object reads over broad object listings.

## Required Call Pattern
- For archive uploads:
  - Use `HeadObject` to compare `x-amz-meta-gitprotect-sha256` with local SHA-256.
  - Skip `PutObject` when hashes match.
  - Only upload when object is missing or hash differs.
- For retention/prune discovery:
  - Use index documents (`indexes/...`) through `GetObject`/`HeadObject`.
  - Do not enumerate entire `backups/` or `mirrors/` prefixes for routine discovery.

## Marker Policy
- Marker writes should be skipped when repository content is unchanged unless explicitly configured otherwise.

## Request Budget Target (Per Repository Run)
- Unchanged archive:
  - `1x HeadObject`
  - `0x archive PutObject`
  - `0x marker PutObject` (default policy)
- Changed archive:
  - `1x HeadObject`
  - `1x archive PutObject`
  - `1x marker PutObject`

## Avoid
- Unconditional archive uploads.
- Unconditional marker rewrites.
- Prefix-wide `ListObjects` scans for normal retention/prune indexing.
