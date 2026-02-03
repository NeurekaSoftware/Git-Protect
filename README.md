# <img src="GitProtect/wwwroot/favicon.png" alt="Git Protect icon" width="28" height="28" /> Git Protect

Git Protect is a self-hosted backup service for Git repositories.

**Getting Started**

Run with Docker Compose using the example below.

```yaml
services:
  git-protect:
    image: ghcr.io/neurekasoftware/git-protect:edge
    container_name: git-protect
    environment:
      - PUID=1000
      - PGID=1000
      - TZ=UTC
      - ASPNETCORE_URLS=http://0.0.0.0:3000
      - GITPROTECT__APIBASEURL=http://127.0.0.1:3000
      - GITPROTECT__CONNECTIONSTRINGS__GITPROTECTDB=Data Source=/data/gitprotect.db
      - GITPROTECT__STORAGE__MIRRORROOT=/data/mirrors
    ports:
      - "3000:3000"
    volumes:
      - ./data:/data
    restart: unless-stopped
```

