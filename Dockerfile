# syntax=docker/dockerfile:1.7

# -----------------------------------------------------------------------------
# Stage 1: build a static, stripped binary.
#
# CGO_ENABLED=0 + netgo means no libc dependency, so we can run on scratch/
# distroless. -trimpath strips absolute paths so the binary is reproducible.
# -----------------------------------------------------------------------------
FROM golang:1.23-alpine AS build

ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=readonly

WORKDIR /src

# Copy module file first; with a lockfile this would be a cache win. We have
# no third-party deps, so this is just future-proofing.
COPY go.mod ./
RUN go mod download

COPY *.go ./

RUN go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION}" \
      -tags netgo,osusergo \
      -o /out/texas-fold-em \
      .

# -----------------------------------------------------------------------------
# Stage 2: distroless static-nonroot.
#
# Image is ~2 MB + our binary. No shell, no package manager, no libc. The
# only mutable path inside the container is whatever volume the k8s manifest
# mounts at TEXAS_FOLDEM_STATE_PATH.
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/texas-fold-em /usr/local/bin/texas-fold-em

# Non-root uid/gid baked into the distroless image.
USER 65532:65532

# Default to a writable path inside a mounted volume. The k8s StatefulSet
# mounts a PVC at /state; see contrib/k8s/statefulset.yaml.
ENV TEXAS_FOLDEM_STATE_PATH=/state/state.json \
    TEXAS_FOLDEM_LISTEN_ADDR=0.0.0.0:8080

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/texas-fold-em"]
