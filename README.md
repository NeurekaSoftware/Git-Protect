# Git Protect

Simple scheduled backup and mirror tool for Git repositories, with S3-compatible object storage support.

## Features

| Backup capability | GitHub | GitLab | Forgejo |
|---|---|---|---|
| Git repository | ✅ | ✅ | ✅ |
| Git LFS objects | ✅ | ✅ | ✅ |
| Issues | ❌ | ❌ | ❌ |
| Issue comments | ❌ | ❌ | ❌ |
| Pull requests / Merge requests | ❌ | ❌ | ❌ |
| PR/MR comments | ❌ | ❌ | ❌ |
| Releases | ❌ | ❌ | ❌ |
| Release artifacts | ❌ | ❌ | ❌ |

| Mirror capability | Status |
|---|---|
| Git repository | ✅ |
| Git LFS objects | ✅ |

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
  pruneOrphanedMirrors: false
  archiveMode: true

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

backups:
  - provider: github
    credential: github
    lfs: false
    enabled: true
  - provider: gitlab
    credential: gitlab
    lfs: false
    enabled: true
  - provider: forgejo
    credential: forgejo
    baseUrl: https://codeberg.org
    lfs: false
    enabled: true

mirrors:
  - url: https://github.com/NeurekaSoftware/Git-Protect
    credential: github
    lfs: true
    force: false
    enabled: true
  - url: https://gitlab.com/gitlab-org/gitlab
    lfs: false
    force: false
    enabled: true

schedule:
  backups:
    cron: "0 0 * * 0"
  mirrors:
    cron: "0 */6 * * *"
```
