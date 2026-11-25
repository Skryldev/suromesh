# SuroMesh Service — multi-stage build (Phase 25 §17; Phase 27 runtime
# hardened further). Runtime: scratch (truly minimal), non-root numeric
# UID, read-only-rootfs compatible, CA certificates included.
#
# Registry note: this environment's egress blocks gcr.io (distroless),
# ghcr.io and cgr.dev; the pinned bases below come from docker.io, which
# is reachable. scratch is stricter than distroless static: no libc, no
# shell, no package DB — only the binary, CA certs, and metadata.

# Build stage: pinned to the repo's declared toolchain (go.mod: go 1.26).
FROM golang:1.26-alpine AS build
WORKDIR /sdkroot

# Layout mirrors the checkout EXACTLY so the service's development
# replace directive (`replace ... => ../`) resolves: SDK module at
# /sdkroot, service module at /sdkroot/service. (The SDK root module has
# ZERO external dependencies — pure stdlib — so no root go.sum exists.)
COPY go.mod ./
COPY client/ ./client/
COPY session/ ./session/
COPY tl/ ./tl/
COPY transport/ ./transport/
COPY crypto/ ./crypto/
COPY auth/ ./auth/
COPY mtproto/ ./mtproto/
COPY rpc/ ./rpc/
COPY internal/ ./internal/

COPY service/ ./service/

WORKDIR /sdkroot/service
# Static binary (CGO off): runs on scratch. -trimpath + -s -w: no build
# paths, no symbol table (smaller; no source paths leak into the image).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/suromesh-service ./cmd/service

# Runtime stage: scratch = image contains ONLY the files below.
FROM scratch

# CA certificates (root store) from the builder: required for TLS to the
# upstream and any TLS-enabled PostgreSQL. Go honors SSL_CERT_FILE.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/suromesh-service /usr/local/bin/suromesh-service

# Read-only filesystem: the service writes nothing to disk (sessions are
# in PostgreSQL, encrypted).
ENV SUROMESH_LISTEN_ADDR=":50051" \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

EXPOSE 50051

# Numeric non-root UID/GID (no /etc/passwd on scratch — numeric is the
# contract; 65532 matches the k8s securityContext).
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/suromesh-service"]
