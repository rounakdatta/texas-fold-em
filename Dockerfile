# Stage 1: Build Go binary
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY *.go ./
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
