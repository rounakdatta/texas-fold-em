# Stage 1: Build Go binary
# Bumped to 1.25 — modernc.org/sqlite + pressly/goose pulled in by the
# integration subsystem require it.
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
# Copy the whole tree so embedded migration .sql + UI .html files are
# present at compile time. Build stage is throwaway so the broad copy
# is fine.
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o texas-fold-em .

# Stage 2: Minimal runtime
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/rounakdatta/texas-fold-em"
RUN apk add --no-cache ca-certificates
RUN adduser -D -h /home/tfe tfe
USER tfe
WORKDIR /home/tfe
COPY --from=builder /app/texas-fold-em /usr/local/bin/texas-fold-em
ENV TEXAS_FOLDEM_STATE_PATH=/data/state.json \
    TEXAS_FOLDEM_LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["texas-fold-em"]
