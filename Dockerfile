# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS build
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
RUN --mount=type=cache,target=/go/pkg/mod,id=gomod-$TARGETARCH,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,id=gobuild-$TARGETARCH,sharing=locked \
    CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" -o /out/git-backup . \
 && mkdir -p /empty-data \
 && chown 1000:1000 /empty-data

# The runtime image is a standalone static binary: no shell, no package
# manager, no gosu. The binary starts as root only long enough to take
# ownership of /app/data and drop to the PUID/PGID identity, then receives
# SIGTERM as PID 1.
FROM gcr.io/distroless/static-debian12:nonroot
ARG GIT_TAG=dev
ARG GIT_HASH=unknown
ENV GIT_TAG=${GIT_TAG}
ENV GIT_HASH=${GIT_HASH}
# The stable nonroot tag provides the distroless filesystem, but its default
# user is overridden: application-managed PUID/PGID needs a privileged start
# before the binary drops root itself.
USER 0
# Runtime identity defaults; Compose or the operator may override either value.
ENV PUID=1000
ENV PGID=1000
WORKDIR /app
COPY --from=build --chown=1000:1000 /empty-data /app/data
COPY --from=build /out/git-backup /app/bin/git-backup
# /app/data holds the incremental git-mirror cache; declaring it a volume lets
# the cache survive container recreation instead of forcing a full re-clone
# each run. The image seeds the directory with uid-1000 ownership so a fresh
# named volume inherits it on first mount; startup re-owns it to PUID/PGID.
VOLUME ["/app/data"]

ENTRYPOINT ["/app/bin/git-backup"]
