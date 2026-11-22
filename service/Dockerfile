# SuroMesh Service — multi-stage build (Phase 25 master spec §17).
# Runtime: minimal, non-root, read-only-rootfs compatible.
# No fake upstream, no dev key provider baked in: configuration decides.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads (service module + SDK via the local replace).
COPY go.mod go.sum ./
COPY ../go.mod /sdk/go.mod
# NOTE: docker build context MUST be the repository root with
#   docker build -f service/Dockerfile .
# so the SDK (../) is reachable for the development replace. Release
# builds (no replace) may use service/ as context alone.
RUN true

COPY service/ ./service/
COPY go.mod go.sum /sdk/
COPY client/ /sdk/client/
COPY session/ /sdk/session/
COPY tl/ /sdk/tl/
COPY transport/ /sdk/transport/
COPY crypto/ /sdk/crypto/
COPY auth/ /sdk/auth/
COPY mtproto/ /sdk/mtproto/
COPY rpc/ /sdk/rpc/
COPY internal/ /sdk/internal/

WORKDIR /src/service
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
    -o /out/suromesh-service ./cmd/service

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/suromesh-service /usr/local/bin/suromesh-service

# Read-only filesystem: the service writes nothing to disk (sessions are
# in PostgreSQL, encrypted). Explicit writable paths (if ever needed):
# VOLUME /var/lib/suromesh
ENV SUROMESH_LISTEN_ADDR=":50051"

EXPOSE 50051
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/suromesh-service"]
