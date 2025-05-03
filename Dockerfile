# Stage 1: Building the application
FROM golang:1.22-alpine AS builder

# Installing build dependencies
RUN apk add --no-cache git

# Creating working directory
WORKDIR /app

# Copying only go.mod first
COPY go.mod ./

# Running go mod tidy to create/update go.sum
RUN go mod tidy

# Downloading all dependencies (explicitly)
RUN go mod download all

# Copying source code
COPY . .

# Re-running go mod tidy after copying code
RUN go mod tidy && go mod verify

# Building the application
# Flag CGO_ENABLED=0 for static compilation
# Flag -ldflags="-s -w" for optimizing binary file size
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o audio-streamer .

# Stage 2: Creating the final image
FROM alpine:latest

# Installing basic utilities
RUN apk add --no-cache ca-certificates tzdata curl findutils

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

# Improved HEALTHCHECK - using metrics that are immediately available
HEALTHCHECK --interval=30s --timeout=2s --start-period=5s --retries=3 \
  CMD wget -q -t1 -T2 http://localhost:8000/metrics || exit 1

# Entry point - our entrypoint script
ENTRYPOINT ["/app/entrypoint.sh"]

# Default arguments - running audio-streamer without explicitly specifying directory
CMD ["/app/audio-streamer"] 