<div align="center">

# Git Backup

[![Release](https://img.shields.io/github/v/release/neurekadev/git-backup?style=flat-square&label=Release&color=F43F5E&logo=github&logoColor=F43F5E)](https://github.com/neurekadev/git-backup/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/neurekadev/git-backup/CI.yaml?branch=main&style=flat-square&label=CI&color=8B5CF6&logo=githubactions&logoColor=8B5CF6)](https://github.com/neurekadev/git-backup/actions/workflows/CI.yaml)
[![License](https://img.shields.io/github/license/neurekadev/git-backup?style=flat-square&label=License&color=14B8A6&logo=opensourceinitiative&logoColor=14B8A6)](./LICENSE.md)
[![AI](https://img.shields.io/badge/AI-assisted-5786FE?style=flat-square&logo=deepseek&logoColor=5786FE)](https://github.com/neurekadev/git-backup)
[![Stars](https://img.shields.io/github/stars/neurekadev/git-backup?style=flat-square&label=Stars&color=EAB308&logo=googlegemini&logoColor=EAB308)](https://github.com/neurekadev/git-backup)

Back up repositories, issues, and more from GitHub, GitLab, and Forgejo to your S3-compatible object storage.

</div>

> [!WARNING]
> This software is under active development. Expect breaking changes and incomplete features.

## Quickstart

Download [`compose.yaml`](./compose.yaml) and [`.env.example`](./.env.example).

## Usage

Save `.env.example` as `.env`, then create `config/settings.yaml` next to `compose.yaml` and start the stack:

```sh
docker compose up -d
```

The image is a standalone static binary on a minimal base — no shell, no entrypoint script. It starts as root only long enough to take ownership of `/app/data`, then permanently drops to the `PUID`/`PGID` from `.env` and receives SIGTERM as PID 1, so `docker compose stop` drains in-flight work cleanly.

> [!NOTE]
> `PUID`/`PGID` default to `1000:1000` and are applied on every start, so changing either value only requires `docker compose up -d` — the data volume is re-owned automatically, with no manual `chown`.

> [!NOTE]
> The `config/` directory is mounted rather than the `settings.yaml` file itself: a single-file bind mount pins the container to the original inode, so edits saved with write-and-rename by most editors never reach the container.

> [!TIP]
> Settings support hot reload, so you don't have to restart your container after editing `config/settings.yaml`. The change is picked up within a couple of seconds and logged.

## Features

### Repository Modes

| Mode | Status | Description |
|---|---|---|
| `provider` | ✅ | Back up all repositories for an account from supported Git forges. |
| `url` | ✅ | Back up one or more repositories via direct URL, without forge API discovery. |

### Supported Providers

| Provider | Status |
|---|---|
| GitHub | ✅ |
| GitLab | ✅ |
| Forgejo | ✅ |

### Repository Capabilities

| Capability | GitHub | GitLab | Forgejo | URL mode |
|---|---|---|---|---|
| Git repositories | ✅ | ✅ | ✅ | ✅ |
| Starred repositories | ✅ | ✅ | ✅ | ➖ |
| Git LFS objects | ✅ | ✅ | ✅ | ✅ |
| Issues | ✅ | ✅ | ✅ | ➖ |
| Issue comments | ✅ | ✅ | ✅ | ➖ |
| Pull requests / merge requests | ✅ | ✅ | ✅ | ➖ |
| PR/MR comments | ✅ | ✅ | ✅ | ➖ |
| Issue / PR / MR attachments | ✅ | ✅ | ✅ | ➖ |
| Releases | ✅ | ✅ | ✅ | ➖ |
| Release artifacts | ✅ | ✅ | ✅ | ➖ |
| Gists / Snippets | ✅ | ✅ | ➖ | ➖ |
| Starred gists / snippets | ✅ | ➖ | ➖ | ➖ |

✅ supported · 🚧 on the roadmap · ❌ unsupported · ➖ not applicable

### Protocol Support

| Protocol | Status |
|---|---|
| HTTP / HTTPS | ✅ |
| SSH | ❌ |

## Settings

Place `settings.yaml` inside a `config/` directory next to `compose.yaml` (so the file is at `config/settings.yaml`); `compose` binds that directory read-only into the container.

```yaml
logging:
  logLevel: info

storage:
  endpoint: https://accountid.r2.cloudflarestorage.com
  region: auto
  bucket: git-backup
  accessKeyId: accessKeyId
  secretAccessKey: secretAccessKey
  forcePathStyle: false
  payloadSignatureMode: full
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
    includeStarred: true
    includeSnippets: true
  - mode: provider
    provider: gitlab
    credential: gitlab
    includeIssues: true
    includeIssueArtifacts: true
    includeMergeRequests: true
    includeMergeRequestsArtifacts: true
    includeReleases: true
    includeReleaseArtifacts: true
  - mode: provider
    provider: forgejo
    credential: forgejo
    baseUrl: https://codeberg.org
  - mode: url
    credential: gitlab
    url:
      - https://github.com/neurekadev/git-backup
      - https://code.neureka.dev/git/website

schedule:
  repositories:
    cron: "0 */6 * * *"

concurrency:
  repositories: 1
  metadata: 1

health:
  port: 8080
  bind: localhost
```

Each entry under `repositories` accepts the options below. **Common** options apply to both modes; the rest belong to `mode: provider` or `mode: url`.

**Common**

| Option | Description | Default |
|---|---|---|
| `mode` | `provider` (discover all repositories for an account) or `url` (specific repositories). | *required* |
| `credential` | Credential key (from `credentials`) used to authenticate. Optional for public repositories in url mode. | *required in provider mode* |
| `lfs` | Back up Git LFS objects. | `true` |
| `cache` | Keep a local mirror and `git fetch` it each run for fast subsequent syncs. Set `false` to clone fresh each run and delete the local copy after a successful upload, bounding local disk to one repository at a time. | `true` |
| `enabled` | Set `false` to skip the job. | `true` |

**Provider mode** — `mode: provider`

| Option | Description | Default |
|---|---|---|
| `provider` | Forge to discover: `github`, `gitlab`, or `forgejo`. | *required* |
| `baseUrl` | Base URL of a self-hosted forge instance. | forge's public URL |
| `includeStarred` | Also back up repositories the account has starred. | `false` |
| `includeSnippets` | Also back up your GitHub gists and GitLab snippets. GitHub also backs up starred gists when `includeStarred` is on; GitLab has no starred snippets. | `false` |
| `includeIssues` | Back up issues and their comment threads as JSON. | `false` |
| `includeIssueArtifacts` | Also download files attached to issues and their comments. Requires `includeIssues`. | `false` |
| `includeMergeRequests` | Back up pull/merge requests and their comment threads as JSON. | `false` |
| `includeMergeRequestsArtifacts` | Also download files attached to pull/merge requests and their comments. Requires `includeMergeRequests`. | `false` |
| `includeReleases` | Back up releases (tag, notes, and asset references) as JSON. | `false` |
| `includeReleaseArtifacts` | Also download release asset files. Requires `includeReleases`. | `false` |

> [!NOTE]
> On GitLab, `includeReleaseArtifacts` downloads only asset-link files hosted on your own instance. Auto-generated source archives are skipped (the repository mirror already captures them), and external links are recorded as references without being downloaded.

**URL mode** — `mode: url`

| Option | Description | Default |
|---|---|---|
| `url` | A single repository URL, or a list of repository URLs. | *required* |

Local mirrors for repositories that are removed from the config (or disabled) are cleaned off disk automatically on the next run.

The optional `concurrency` section tunes parallelism and defaults to fully sequential:

| Option | Description | Default |
|---|---|---|
| `concurrency.repositories` | How many repositories a run clones, archives, and uploads in parallel. | `1` |
| `concurrency.metadata` | How many issues/merge requests are fetched (with comments) and uploaded in parallel within one repository. | `1` |

> [!TIP]
> Raising these overlaps network-bound work, but increases concurrent memory, local disk, and provider/S3 request pressure — raise gradually and watch for rate limiting (HTTP 429). With `cache: false`, running repositories in parallel multiplies peak local disk by the degree.

The optional `health` section exposes a small HTTP status endpoint so a stalled scheduler is visible from the outside:

| Option | Description | Default |
|---|---|---|
| `health.port` | Port the status listener binds to; `0` disables it. | `8080` |
| `health.bind` | Interface to listen on. Use `0.0.0.0` inside a container so the port can be published. | `localhost` |

`GET /` and `GET /healthz` return JSON describing the daemon; `status` reads `"ok"` until a run fails and `"degraded"` afterwards, so an Uptime-Kuma-style keyword check can alert on it:

```json
{"status":"ok","startedAt":"2026-09-09T20:29:39Z","lastRunStartedAt":null,"lastRunCompletedAt":null,"lastRunDurationSeconds":null,"lastRunOutcome":null,"nextRunAt":"2027-01-01T08:00:00Z"}
```

To publish it, add a mapping to `compose.yaml` and set the bind address:

```yaml
services:
  git-backup:
    ports:
      - "8080:8080"
```

```yaml
health:
  port: 8080
  bind: 0.0.0.0
```

## Credentials

Each key under `credentials` is a forge token, referenced by a repository job's `credential`. Every operation this tool performs is read-only, so create each token with the least privilege the features you enable require.

**GitHub** — create a **classic** personal access token; fine-grained tokens cannot read gists.

| Scope | Needed for |
|---|---|
| `repo` | Private repositories, plus their issues, pull requests, comments, releases, and attachments |
| `gist` | Gists (`includeSnippets`) |

Backing up only public repositories? `public_repo` replaces `repo`. Not backing up gists? Omit `gist`.

**GitLab** — create a personal access token with:

| Scope | Needed for |
|---|---|
| `read_api` | Discovering repositories and reading issues, merge requests, releases, and snippets |
| `read_repository` | Cloning repository contents, including private repositories and LFS objects |

**Forgejo** — create an access token granting read access to:

| Scope | Needed for |
|---|---|
| `read:repository` | Repositories, starred repositories, pull requests, releases, and attachments |
| `read:issue` | Issues and issue / pull-request comment threads |

### Keeping secrets out of settings.yaml

`storage.accessKeyId`, `storage.secretAccessKey`, and each `credentials.*.apiKey` accept a literal value, a `${ENV_VAR}` placeholder, or a companion `*File` key naming a file to read. Anyone who can read `settings.yaml` can otherwise recover every forge token and your storage keys at once, so prefer one of the indirections outside local testing.

```yaml
storage:
  # Read from the container environment — pairs with the .env file compose already loads.
  accessKeyId: ${S3_ACCESS_KEY_ID}
  secretAccessKey: ${S3_SECRET_ACCESS_KEY}

credentials:
  gitlab:
    username: git
    # Or read from a file, such as a Docker/Kubernetes secret mount.
    apiKeyFile: /run/secrets/gitlab_token
```

A referenced variable that is not set, a file that cannot be read, or a field given both ways is rejected at load rather than started up without.

> [!IMPORTANT]
> Issues, pull/merge requests, releases, and their attachments are backed up for **owned** repositories only. They are never fetched for **starred** repositories — even when `includeStarred` is enabled — nor for gists or snippets.

## Backup Storage

Issues, pull/merge requests, and releases are stored as latest-state JSON documents (issues and MRs each embed their comment thread) next to the repository's Git snapshots. Each run overwrites these in place and removes documents for items that no longer exist upstream.

| Item | Stored at |
|---|---|
| Issues | `issues/{number}.json` |
| Pull / merge requests | `merge-requests/{number}.json` |
| Releases | `releases/{tag}.json` |
| Per-collection manifest | `{collection}/index.json` |
| Downloaded attachments | `{collection}/attachments/{id}/` |

## Why Use Git Backup?

Point it at your GitHub, GitLab, or Forgejo account once and it keeps running a scheduled mirror of everything you own: repositories, Git LFS objects, issues, pull requests, releases, and attachments, all stored in your own S3-compatible bucket. Backups use a local mirror cache so consecutive runs are fast and cheap on both ends, and the JSON documents are plain, readable snapshots you can browse or process without any vendor tooling.
