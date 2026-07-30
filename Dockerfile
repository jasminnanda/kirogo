# kirogo container image.
#
# The binary needs no container to run — a single static executable is still the
# simplest way to deploy kirogo. This image exists for hosts that standardise on
# containers, where being the one process managed differently is its own cost.
#
# Build:
#   docker build --build-arg VERSION=1.0.2 -t kirogo:1.0.2 .
#
# Run:
#   docker run -d --name kirogo -p 127.0.0.1:8000:8000 \
#     -e PROXY_API_KEY=... \
#     -v /var/lib/kirogo/creds:/creds \
#     -e KIRO_CREDS_FILE=/creds/kiro-auth-token.json \
#     kirogo:1.0.2

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.24-alpine AS build

WORKDIR /src

# go.mod has no require block, so there is nothing to download and no separate
# dependency-caching layer worth having. Copying the module files first still
# lets `go mod verify` fail early if the module declaration is malformed.
COPY go.mod ./
RUN go mod verify

COPY cmd ./cmd
COPY internal ./internal

# VERSION is stamped into the binary so `-version` and /health cannot lie about
# what is running. Left at "dev" the config package falls back to Go build info,
# which reports a pseudo-version for a tree with no VCS metadata — accurate, but
# not useful in a deployment.
ARG VERSION=dev

# CGO off: the point is a binary with no shared-library dependencies, so it runs
# on a base image that contains nothing but a CA bundle.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build \
        -trimpath \
        -ldflags="-s -w -X github.com/jasminnanda/kirogo/internal/config.Version=${VERSION}" \
        -o /out/kirogo \
        ./cmd/kirogo

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
#
# Alpine rather than scratch or distroless, for one concrete reason: a container
# that cannot answer HEALTHCHECK looks identical whether it is serving traffic or
# wedged with an open socket. busybox wget gives a real liveness probe for about
# 8 MB, and leaves a shell for diagnosing the container that is actually failing.
FROM alpine:3.21

# ca-certificates is not optional: every call kirogo makes upstream is HTTPS, and
# without a trust store the first token refresh fails on certificate validation.
# tzdata keeps log timestamps interpretable when TZ is set.
RUN apk add --no-cache ca-certificates tzdata

# Unprivileged and numeric. A numeric UID means the container does not depend on
# passwd lookups, and matches the ownership a host bind mount has to grant.
RUN addgroup -g 65532 -S kirogo \
    && adduser -u 65532 -S -G kirogo -H -s /sbin/nologin kirogo

COPY --from=build /out/kirogo /usr/local/bin/kirogo

# Defaults chosen for the container case rather than the host case: binding the
# container's own 0.0.0.0 is what makes -p work, and it is not a public exposure
# because publishing is what decides reachability. Bind to 127.0.0.1 on the host
# side of -p, or put a TLS terminator in front.
ENV SERVER_HOST=0.0.0.0 \
    SERVER_PORT=8000

EXPOSE 8000
USER 65532:65532

# /health needs no credentials, so the probe cannot be defeated by a bad key and
# needs no secret baked into the image.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8000/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/kirogo"]
