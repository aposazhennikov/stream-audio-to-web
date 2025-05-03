# Audio Streaming Server on Go

**Made by Aleksandr Posazhennkiov DevOps Giftery**

High-performance server for streaming audio files to a browser, written in Go. The server has minimal memory usage, supports multiple audio formats, and provides synchronized "radio-like" playback for all connected clients.

## Features

- **Memory < 100 MB** — even when streaming tracks several hours long
- **Synchronized "real radio"** — all listeners hear the same content at the same time
- **Multiple "stations"** — mapping URL paths to independent directories (e.g., `/humor`, `/news`)
- **On-the-fly playlist reload** — add or remove media files without restarting the server
- **Docker image < 20 MB** — multi-stage build with health check
- **Support for MP3, AAC, OGG formats**
- **Efficient memory usage** thanks to the use of `io.CopyBuffer` and `sync.Pool`
- **Performance monitoring** through Prometheus and Sentry
- **Graceful shutdown** upon receiving signals
- **Automatic access rights handling** — works even with files owned by root
- **Optional track shuffling** — can be enabled or disabled
- **Status page with authorization** — protected access to information about streams and player control
- **Manual player control** — ability to switch tracks forward and backward through the web interface
- **Track history tracking** — track history is available for each station

## Requirements

- Go 1.22 or higher
- Docker Engine for building Docker image

## Project Structure

The project has a modular architecture with a clear separation of responsibilities:

### Main Components

1. **`main.go`** - The main application file, contains the entry point, command line arguments processing, and initialization of all components.

2. **`audio/`** - Package for working with audio data
   - `streamer.go` - Contains the logic for streaming audio files, manages client connections and data buffering.

3. **`playlist/`** - Package for playlist management
   - `playlist.go` - Responsible for scanning directories, tracking changes in the file system, shuffling tracks, playback history.

4. **`http/`** - Package for HTTP server
   - `server.go` - Implements an HTTP server, handles requests, manages streams, and registers routes.

5. **`radio/`** - Package for managing "radio stations"
   - `radio.go` - Links playlists and audio streamers, manages playback streams.

6. **`web/`** - Web interface
   - `index.html` - HTML page with an audio player and JavaScript for interacting with the server.

7. **`entrypoint.sh`** - Script for handling access rights to audio files before launching the application.

### Additional Files

- `go.mod` / `go.sum` - Go dependency management files
- `Dockerfile` - Instructions for building a Docker image
- `docker-compose.yml` - Configuration for running via Docker Compose
- `kubernetes.yaml` - Manifest for deploying to Kubernetes

### Component Interaction

1. `main.go` loads the configuration and initializes the server
2. For each route, a `playlist.Playlist` object is created, which scans the corresponding directory
3. For each playlist, an `audio.Streamer` is created, which is responsible for reading and sending audio data
4. `radio.RadioStation` links playlists and streamers for continuous playback
5. `http.Server` creates HTTP handlers for accessing streams

## Access Rights Handling

The application automatically checks and corrects access rights to audio files at startup. The process works as follows:

1. When the container starts, the `entrypoint.sh` script is executed first
2. The script checks all mounted directories with audio files
3. If files without read permissions are detected, the script automatically adds the necessary permissions
4. This allows working with files owned by the root user without manual configuration

## Configuration

The server can be configured through command line flags or environment variables:

| Flag / ENV           | Purpose                                    | Default   |
|----------------------|--------------------------------------------|-----------|
| `--port` / `PORT`    | HTTP server port                           | `8000`    |
| `--audio-dir` / `AUDIO_DIR` | Default audio files directory        | `./audio` |
| `--stream-format` / `STREAM_FORMAT` | Stream format (mp3, aac, ogg) | `mp3`     |
| `--bitrate` / `BITRATE` | Target bitrate in kbps                  | `128`     |
| `--max-clients` / `MAX_CLIENTS` | Maximum number of simultaneous clients | `500` |
| `--log-level` / `LOG_LEVEL` | Logging level (debug, info, warn, error) | `info` |
| `--buffer-size` / `BUFFER_SIZE` | Read buffer size in bytes        | `65536` (64KB) |
| `--directory-routes` / `DIRECTORY_ROUTES` | JSON string with route-directory mapping | `{}` |
| `--shuffle` / `SHUFFLE` | Enable/disable track shuffling         | `false`   |
| (no flag) / `STATUS_PASSWORD` | Password for accessing the status page | `1234554321` |
| (no flag) / `SENTRY_DSN` | Sentry DSN for error tracking | Empty (disabled) |

## Monitoring and Observability

The server provides the following endpoints for monitoring:

- **/healthz** — instantaneous 200 OK response if the server is running
- **/readyz** — checks free disk space, RAM usage, and directory availability
- **/metrics** — Prometheus metrics (number of listeners, bytes transferred, etc.)
- **/status** — password-protected page with status and control for all audio streams

## Running the Application

### Building from Source

1. Clone the repository
```bash
git clone https://github.com/user/stream-audio-to-web.git
cd stream-audio-to-web
```

2. Build the application
```bash
go build -o audio-streamer .
```

3. Run with default settings
```bash
./audio-streamer --audio-dir ./audio
```

4. Run with multiple "stations" from different directories
```bash
./audio-streamer --audio-dir ./audio --directory-routes '{"humor":"/home/humor","science":"/home/science"}'
```

When run with these settings, the server will create the following routes:
- `:8000/` - broadcasting audio from the `./audio` directory
- `:8000/humor` - broadcasting audio from the `/home/humor` directory
- `:8000/science` - broadcasting audio from the `/home/science` directory

When connecting to these routes in the browser, the audio stream will start automatically.

### Building and Running with Docker

1. Clone the repository
```bash
git clone https://github.com/user/stream-audio-to-web.git
cd stream-audio-to-web
```

2. Build Docker image
```bash
docker build -t audio-streamer:latest .
```

3. Run the container with audio directories
```bash
docker run -d --name audio-streamer \
  -p 8000:8000 \
  -v /path/to/audio:/app/audio \
  -v /path/to/humor:/app/humor \
  -v /path/to/science:/app/science \
  -e DIRECTORY_ROUTES='{"humor":"/app/humor","science":"/app/science"}' \
  -e SHUFFLE=false \
  -e STATUS_PASSWORD=your_password \
  -e SENTRY_DSN=your_sentry_dsn \
  audio-streamer:latest
```

> **Note**: The container automatically handles access rights to audio files, so it works even with files owned by the root user.

4. Check container status
```bash
docker ps
```

5. View logs
```bash
docker logs audio-streamer
```

6. Stop the container
```bash
docker stop audio-streamer
```

### With Docker Compose

1. Create a `docker-compose.yml` file:
```yaml
version: '3.8'

services:
  audio-streamer:
    image: audio-streamer:latest
    # For working with files owned by root
    privileged: true
    ports:
      - "8000:8000"
    volumes:
      - ./audio:/app/audio
      - ./humor:/app/humor
      - ./science:/app/science
    environment:
      - STREAM_FORMAT=mp3
      - BITRATE=128
      - MAX_CLIENTS=500
      - LOG_LEVEL=info
      - BUFFER_SIZE=65536
      - DIRECTORY_ROUTES={"humor":"/app/humor","science":"/app/science"}
      - SHUFFLE=false
      - STATUS_PASSWORD=your_password
      - SENTRY_DSN=your_sentry_dsn
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/healthz"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 5s
    deploy:
      resources:
        limits:
          cpus: '1'
          memory: 256M
    restart: unless-stopped
```

2. Start
```bash
docker-compose up -d
```

3. View logs
```bash
docker-compose logs -f
```

4. Stop
```bash
docker-compose down
```

## API and Endpoints

- **`/streams`** — list of all available audio streams in JSON format
- **`/now-playing`** — information about the current track
- **`/reload-playlist`** — playlist reload
- **`/web`** — web interface with audio players
- **`/<route>`** — endpoint for listening to the audio stream
- **`/status`** — password-protected stream status page with playback control capabilities
- **`/next-track/<route>`** — move to the next track for the specified route
- **`/prev-track/<route>`** — move to the previous track for the specified route

## Status Page and Playback Control

The server has a built-in web interface for monitoring and controlling audio streams, accessible at `/status`.

### Status Page Features:

- **Protected access** — login with a password set through the `STATUS_PASSWORD` environment variable
- **Status information** — display of current track, start time, and number of listeners for each station
- **Player control** — buttons for switching tracks forward and backward
- **Track history** — list of recently played tracks for each station (up to 100 tracks)
- **Stable station order** — stations are always displayed in the same order (alphabetical)

### Using the Status Page:

1. Open `http://server:port/status` in your browser
2. Enter the password (default `1234554321` or set via `STATUS_PASSWORD`)
3. After authorization, you will see a list of all registered audio streams
4. For each stream, the following are available:
   - "Switch back" button — go to the previous track
   - "Switch forward" button — go to the next track
   - "Show track history" button — open a list of recently played tracks

## Support for Different Directories for Different Routes

The server supports mapping URL routes and directories with audio files. This allows you to organize several "radio stations", each broadcasting its own content:

```
/home/humor    →  http://localhost:8000/humor    (humorous content)
/home/science  →  http://localhost:8000/science  (scientific content)
./audio        →  http://localhost:8000/         (main content)
```

To configure the mapping, you can use:

1. Command line flag:
```bash
./audio-streamer --directory-routes '{"humor":"/home/humor","science":"/home/science"}'
```

2. Environment variable:
```bash
export DIRECTORY_ROUTES='{"humor":"/home/humor","science":"/home/science"}'
./audio-streamer
```

3. In Docker:
```bash
docker run -d -p 8000:8000 \
  -v /home/humor:/app/humor \
  -v /home/science:/app/science \
  -e DIRECTORY_ROUTES='{"humor":"/app/humor","science":"/app/science"}' \
  -e SHUFFLE=false \
  -e STATUS_PASSWORD=your_password \
  -e SENTRY_DSN=your_sentry_dsn \
  audio-streamer:latest
```

## Architecture

The project has a modular architecture with a clear separation of responsibilities:

- **audio** — managing audio streams and client connections
- **playlist** — scanning directories, managing playlists and track history
- **http** — HTTP server, request handlers, and status page
- **radio** — managing "radio stations" and track playback

## Performance

- **RAM < 100 MB** even when serving hundreds of clients
- **CPU < 5%** on modern servers
- **Docker image size < 20 MB**
- **Peak load ~1000 parallel clients** (depends on the server)

## License

MIT


# TODO 


- Fix shuffle mode, it doesn't work right now. - NOT DONE ❌

- Check how it's work if we addin new audio to folder, while stream working. - NOT DONE ❌

- Add routes to switch audio forward/backward(It should be available by curl with specific header, to protect from random internet scanners) - NOT DONE ❌

- Check how circle play working, after last audio in playlist should play first one  - DONE ✅

- Add HEAD requeests for monitoring DONE ✅
