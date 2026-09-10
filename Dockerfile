# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
ARG TARGETOS
ARG TARGETARCH
ARG GIT_TAG=dev
ARG GIT_HASH=unknown

WORKDIR /src
# Resolve dependencies on their own layer so a source-only change reuses the
# module cache instead of re-resolving every package. The per-arch cache id
# stops the amd64 and arm64 stages of a multi-platform build from serializing
# behind one shared cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=gomod-$TARGETARCH,sharing=locked \
    go mod download

COPY . .
# The main package imports time/tzdata, so the IANA timezone database is
# embedded and TZ works without a system tzdata package in the scratch runtime.
RUN --mount=type=cache,target=/go/pkg/mod,id=gomod-$TARGETARCH,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-$TARGETARCH,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /out/git-backup . \
 && mkdir -p /out/data

# The runtime image is a standalone static binary: no shell, no package
# manager, no entrypoint script. It runs as root (the container default) and
# receives SIGTERM as PID 1.
FROM scratch
ARG GIT_TAG=dev
ARG GIT_HASH=unknown
ENV GIT_TAG=${GIT_TAG}
ENV GIT_HASH=${GIT_HASH}
WORKDIR /app
# Forge APIs, git remotes, and object storage are all reached over TLS.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# /app/data holds the incremental git-mirror cache; declaring it a volume lets
# the cache survive container recreation instead of forcing a full re-clone
# each run.
COPY --from=build /out/data /app/data
COPY --from=build /out/git-backup /app/bin/git-backup
VOLUME ["/app/data"]

ENTRYPOINT ["/app/bin/git-backup"]
