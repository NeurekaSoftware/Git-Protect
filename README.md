# GitProtect

GitProtect is a self-hosted backup service for Git repositories.

**Getting Started**

Run with Docker Compose using the example below.

```yaml
services:
  gitprotect:
    image: ghcr.io/neurekasoftware/git-protect:edge
    container_name: gitprotect
    environment:
      ASPNETCORE_URLS: http://0.0.0.0:3000
      PUID: "1000"
      PGID: "1000"
      TZ: "UTC"
      GITPROTECT__CONNECTIONSTRINGS__GITPROTECTDB: "Data Source=/data/gitprotect.db"
      GITPROTECT__STORAGE__MIRRORROOT: "/data/mirrors"
    ports:
      - "3000:3000"
    volumes:
      - ./data:/data
    restart: unless-stopped
```


