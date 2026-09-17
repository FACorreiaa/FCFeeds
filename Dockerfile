# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /app
ENV GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go SQLite (modernc.org/sqlite) means no cgo and a static binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" -o feeds ./cmd/feeds

# Runtime stage
FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata \
    && addgroup -S -g 1000 feeds && adduser -S -u 1000 -G feeds -H feeds \
    && mkdir -p /data && chown feeds:feeds /data
WORKDIR /app
COPY --from=builder /app/feeds .
# SQLite database lives here; mount a volume.
VOLUME /data
EXPOSE 8080
USER feeds
ENTRYPOINT ["/app/feeds"]
