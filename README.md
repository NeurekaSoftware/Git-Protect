# Git Protect

Simple scheduled repository snapshot tool for Git repositories, with S3-compatible object storage support.
Repository snapshots are stored as `tar.gz` archives.

## Features

| Repository mode | Status | Notes |
|---|---|---|
| `provider` | ✅ | Discover owned repositories from GitHub, GitLab, or Forgejo API. |
| `url` | ✅ | Track any repository URL directly without forge API discovery. |

| Repository capability | GitHub | GitLab | Forgejo | URL mode |
|---|---|---|---|---|
| Git repository | ✅ | ✅ | ✅ | ✅ |
| Git LFS objects | ✅ | ✅ | ✅ | ✅ |
| Issues | ❌ | ❌ | ❌ | ❌ |
| Issue comments | ❌ | ❌ | ❌ | ❌ |
| Pull requests / Merge requests | ❌ | ❌ | ❌ | ❌ |
| PR/MR comments | ❌ | ❌ | ❌ | ❌ |
| Releases | ❌ | ❌ | ❌ | ❌ |
| Release artifacts | ❌ | ❌ | ❌ | ❌ |

| Protocol support | Status |
|---|---|
| HTTP/HTTPS | ✅ |
| SSH | ❌ |

## Quick Start

### Docker Compose

```yaml
services:
  git-protect:
    image: ghcr.io/neurekasoftware/git-protect:edge
    container_name: git-protect
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

### settings.yaml

> [!TIP]
> These settings support hot reload so you don't have to restart your container after making changes.

```yaml
logging:
  logLevel: info

storage:
  endpoint: https://accountid.r2.cloudflarestorage.com
  region: auto
  bucket: git-protect
  accessKeyId: accessKeyId
  secretAccessKey: secretAccessKey
  forcePathStyle: false
  payloadSignatureMode: full
  alwaysCalculateContentMd5: false
  retention: 30
  retentionMinimum: 1

credentials:
  github:
    username: git
    apiKey: githubToken
  gitlab:
    username: git
    apiKey: gitlabToken
  forgejo:
    username: git
    apiKey: forgejoToken

repositories:
  - mode: provider
    provider: github
    credential: github
    lfs: false
    enabled: true
  - mode: provider
    provider: gitlab
    credential: gitlab
    lfs: false
    enabled: true
  - mode: provider
    provider: forgejo
    credential: forgejo
    baseUrl: https://codeberg.org
    lfs: false
    enabled: true
  - mode: url
    url: https://github.com/NeurekaSoftware/Git-Protect
    credential: github
    lfs: true
    enabled: true
  - mode: url
    url: https://gitlab.com/gitlab-org/gitlab
    lfs: false
    enabled: true

schedule:
  repositories:
    cron: "0 */6 * * *"
```

## Migration Note

The old `backups` and `mirrors` config keys are no longer supported.
Use `repositories` with `mode: provider` or `mode: url`.
