# Stage 1: Building the application
FROM golang:1.22-alpine3.20 AS builder

# Installing build dependencies
RUN apk add --no-cache git

# Creating working directory
WORKDIR /app

# Copying go.mod and go.sum first, so the module download is cached until they change
COPY go.mod go.sum ./

# Downloading dependencies at the exact versions pinned in go.sum
RUN go mod download

# Copying source code
COPY . .

# Build information shown on the web page and at /version (passed by CI)
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_TIME=""

# Building the application
# Flag CGO_ENABLED=0 for static compilation
# Flag -trimpath removes local build paths from the binary
# Flag -ldflags="-s -w" for optimizing binary file size, -X sets the build information
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w \
      -X github.com/aposazhennikov/stream-audio-to-web/version.Version=${VERSION} \
      -X github.com/aposazhennikov/stream-audio-to-web/version.Commit=${COMMIT} \
      -X github.com/aposazhennikov/stream-audio-to-web/version.BuildTime=${BUILD_TIME}" \
    -o audio-streamer .

# Stage 2: Creating the final image
FROM alpine:3.20

# Installing basic utilities and FFmpeg for accurate MP3 duration detection
RUN apk add --no-cache ca-certificates tzdata curl findutils ffmpeg

# Copying binary file from build stage
COPY --from=builder /app/audio-streamer /app/
COPY --from=builder /app/web /app/web
COPY --from=builder /app/templates /app/templates
COPY --from=builder /app/image /app/image

# Copying entrypoint script
COPY entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

# Creating directory for audio files
RUN mkdir -p /app/audio /app/humor /app/science && \
    chmod -R 755 /app

# Working directory
WORKDIR /app

# Opening port
EXPOSE 8000

# Improved HEALTHCHECK - using readyz endpoint with grep check for OK
HEALTHCHECK --interval=30s --timeout=2s --start-period=5s --retries=3 \
  CMD curl -s http://localhost:8000/healthz | grep -q 'OK'

# Entry point - our entrypoint script
ENTRYPOINT ["/app/entrypoint.sh"]

# Default arguments - running audio-streamer without explicitly specifying directory
CMD ["/app/audio-streamer"]
