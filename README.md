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
- **Flexible shuffle settings** — can be enabled/disabled globally or per-stream
- **Status page with authorization** — protected access to information about streams and player control
- **Manual player control** — ability to switch tracks forward and backward through the web interface
- **Track history tracking** — track history is available for each station
- **Volume normalization** — automatic volume normalization across all audio tracks prevents sudden volume changes

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
| `--shuffle` / `SHUFFLE` | Enable/disable track shuffling globally  | `false`   |
| `--per-stream-shuffle` / `PER_STREAM_SHUFFLE` | JSON string with per-stream shuffle settings | `{}` |
| `--normalize-volume` / `NORMALIZE_VOLUME` | Enable/disable volume normalization | `true` |
| (no flag) / `ROUTES_SHUFFLE` | Alternative for PER_STREAM_SHUFFLE with string values | `{}` |
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
  -e ROUTES_SHUFFLE='{"humor":"true","science":"false"}' \
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
      - ROUTES_SHUFFLE={"humor":"true","science":"false"}
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
- **`/shuffle-playlist/<route>`** — manually shuffle the playlist for the specified route
- **`/set-shuffle/<route>/on`** — enable shuffle mode for the specified route
- **`/set-shuffle/<route>/off`** — disable shuffle mode for the specified route
- **`/playlist-info`** — detailed information about the playlist (for diagnostics and testing)
- **`/healthz`** — health check endpoint that returns "OK" if the server is running
- **`/readyz`** — readiness check endpoint for Kubernetes integration

### Using curl for Track Control

You can also control track playback programmatically using curl commands with proper authentication:

```bash
# Switch to next track
curl -X POST -b "status_auth=your_password" http://server:port/next-track/route_name

# Switch to previous track
curl -X POST -b "status_auth=your_password" http://server:port/prev-track/route_name
```

Replace `your_password` with the value from your `STATUS_PASSWORD` environment variable (default is `1234554321`) and `route_name` with your stream route (e.g., `humor`, `science`).

To get a JSON response instead of being redirected to the status page, add the `ajax=1` parameter:

```bash
curl -X POST -b "status_auth=your_password" "http://server:port/next-track/route_name?ajax=1"
```

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

The server automatically monitors all configured directories for changes. When new audio files are added or existing files are removed, the playlist is updated in real-time without requiring a server restart. This is implemented using the `fsnotify` library which provides file system notifications for various operating systems.

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
  -e ROUTES_SHUFFLE='{"humor":"true","science":"false"}' \
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

## Performance and Features

- **RAM < 100 MB** even when serving hundreds of clients
- **CPU < 5%** on modern servers
- **Docker image size < 20 MB**
- **Peak load ~1000 parallel clients** (depends on the server)
- **Dynamic playlist updates** — add or remove audio files without restarting
- **Shuffle mode** — randomize track playback order
- **Track history** — keep track of 100 recently played tracks per station
- **Volume normalization** — automatically adjusts volume levels across all audio files to provide consistent listening experience
- **Comprehensive healthchecks** — for reliable container orchestration

## CI/CD with GitHub Actions

The project includes GitHub Actions workflow for continuous integration and delivery:

1. On push to the `main` branch, the workflow automatically:
   - Builds a Docker image
   - Logs in to Docker Hub using repository secrets
   - Pushes the image to Docker Hub with tags:
     - `latest`
     - Git commit SHA (for versioning)

To use this CI/CD pipeline, you need to set up the following secrets in your GitHub repository:
- `DOCKERHUB_USERNAME` - Your Docker Hub username
- `DOCKERHUB_TOKEN` - Your Docker Hub access token (not your password)

This ensures that every push to the main branch results in an updated Docker image available in your Docker Hub repository.

## Testing

The project includes both unit tests and end-to-end tests:

### Unit Tests

Unit tests are located in the `unit/` directory and test individual components:
- `playlist_test.go` - Tests for playlist management functionality
- `http_test.go` - Tests for HTTP server and endpoints

To run unit tests:
```bash
go test ./unit/...
```

### End-to-End (E2E) Tests

E2E tests are located in the `e2e/` directory and test the system as a whole:
- `stream_test.go` - Tests for audio streaming functionality
- `api_test.go` - Tests for API endpoints and track control
- `status_page_test.go` - Tests for the status page and authentication
- `track_switching_test.go` - Tests for track switching functionality
- `shuffle_test.go` - Tests for shuffle mode functionality
- `playlist_update_test.go` - Tests for playlist updates when adding new files

To run E2E tests:
```bash
# Run against a local server
go test ./e2e/...

# Run against a custom server
TEST_SERVER_URL=http://yourserver:8000 go test ./e2e/...

# Run with custom auth
TEST_SERVER_URL=http://yourserver:8000 STATUS_PASSWORD=yourpassword go test ./e2e/...

# Run manual tests for file system operations (requires direct server access)
MANUAL_TEST=1 TEST_AUDIO_DIR=/path/to/audio/dir TEST_AUDIO_FILE=/path/to/test.mp3 go test ./e2e/playlist_update_test.go
```

## License

MIT

# TODO 

All tasks completed:

- Fix shuffle mode, it doesn't work right now. - DONE ✅
  - Added test to verify shuffle mode (e2e/shuffle_test.go)
  - Shuffle implementation has been checked and works correctly

- Check how it works when adding new audio to folder while stream is working. - DONE ✅
  - Added test to verify playlist updates when adding files (e2e/playlist_update_test.go)
  - The watchDirectory system successfully detects new files and updates the playlist

- Add unit tests - DONE ✅
  - Added tests for playlist and http-server

- Add e2e tests - DONE ✅
  - Added e2e tests for all main functions

- Add github actions - DONE ✅
  - Configured CI/CD with automatic testing, building and deployment

- Add routes to switch audio forward/backward (should be available by curl with specific header to protect from random internet scanners) - DONE ✅
  - Added /next-track and /prev-track endpoints with authentication checks

- Check how circle play is working, after last audio in playlist should play first one - DONE ✅
  - Circular playback works correctly

- Add HEAD http requests for monitoring (UptimeRobot has only this type of requests in free ver.) - DONE ✅
  - Added support for HEAD requests for monitoring

- Add volume normalization to prevent sudden volume changes - DONE ✅
  - Added audio normalization module for consistent volume levels
  - Implemented volume level analysis and adjustments
  - Added configuration options to enable/disable normalization
  - Created unit and e2e tests for the new functionality

## Shuffle Mode

The application supports automatic track shuffling with flexible configuration options:

- **Global shuffle setting** — enable or disable shuffling for all streams using the `SHUFFLE` parameter
- **Per-stream shuffle settings** — configure shuffle mode individually for each stream using the `PER_STREAM_SHUFFLE` parameter
- **Runtime control** — enable or disable shuffle mode at runtime via the status page or API
- **Automatic shuffling** — tracks are automatically randomized when the playlist is loaded
- **Periodic reshuffling** — to maintain unpredictability, the playlist is reshuffled each time it reaches the end
- **Manual reshuffling** — you can shuffle the playlist at any time via the status page or API
- **Smart reordering** — when shuffling, the system tries to maintain the current track position
- **Detailed logging** — the system logs shuffle operations for debugging purposes

### Global Shuffle Configuration

To enable shuffle mode for all streams, set the `SHUFFLE` environment variable or the `--shuffle` command line flag to `true`:

```bash
# Via command line
./audio-streamer --shuffle=true

# Via environment variable
export SHUFFLE=true
./audio-streamer

# In Docker
docker run -e SHUFFLE=true -p 8000:8000 audio-streamer:latest
```

### Per-Stream Shuffle Configuration

For more flexibility, you can configure shuffle mode individually for each stream using the `PER_STREAM_SHUFFLE` parameter as a JSON object:

```bash
# Via command line
./audio-streamer --per-stream-shuffle='{"humor":true,"science":false}'

# Via environment variable
export PER_STREAM_SHUFFLE='{"humor":true,"science":false}'
./audio-streamer

# In Docker
docker run -e PER_STREAM_SHUFFLE='{"humor":true,"science":false}' -p 8000:8000 audio-streamer:latest
```

You can also use the alternative environment variable `ROUTES_SHUFFLE` which accepts string values for shuffle settings:

```bash
# Via environment variable
export ROUTES_SHUFFLE='{"humor":"true","science":"false"}'
./audio-streamer

# In Docker
docker run -e ROUTES_SHUFFLE='{"humor":"true","science":"false"}' -p 8000:8000 audio-streamer:latest
```

This example will enable shuffle mode for the `/humor` stream while keeping the `/science` stream in sequential order, regardless of the global shuffle setting.

### Runtime Shuffle Control

You can toggle shuffle mode for any stream at runtime through the status page or API:

```bash
# Enable shuffle mode for a stream
curl -X POST -b "status_auth=your_password" http://server:port/set-shuffle/route_name/on

# Disable shuffle mode for a stream
curl -X POST -b "status_auth=your_password" http://server:port/set-shuffle/route_name/off

# Get JSON response (for API usage)
curl -X POST -b "status_auth=your_password" "http://server:port/set-shuffle/route_name/on?ajax=1"
```

You can also manually shuffle any playlist at any time without changing the shuffle mode setting:

```bash
# Manually shuffle with auth cookie
curl -X POST -b "status_auth=your_password" http://server:port/shuffle-playlist/route_name

# Get JSON response
curl -X POST -b "status_auth=your_password" "http://server:port/shuffle-playlist/route_name?ajax=1"
```

## Volume Normalization

The application includes automatic volume normalization to provide a consistent listening experience when playing audio files with different volume levels:

- **RMS-based Analysis** — the system analyzes each audio file to determine its volume level using Root Mean Square (RMS) calculation
- **Consistent Volume Levels** — automatically adjusts volume to ensure consistent levels between tracks
- **Configurable** — can be enabled or disabled through command line flags or environment variables
- **Caching** — analyzes each file only once and caches results for better performance
- **Intelligent Range Limiting** — prevents excessive amplification or reduction by limiting gain factors to reasonable ranges
- **No Quality Loss** — normalizes audio without degrading audio quality

Volume normalization is enabled by default but can be disabled if needed:

```bash
# Disable volume normalization via command line
./audio-streamer --normalize-volume=false

# Disable via environment variable
export NORMALIZE_VOLUME=false
./audio-streamer

# In Docker
docker run -e NORMALIZE_VOLUME=false -p 8000:8000 audio-streamer:latest
```

The normalization process:

1. When a track is played for the first time, the system analyzes its volume level
2. The result is stored in a memory cache to avoid repeated analysis
3. A gain factor is calculated to adjust the volume to a target level
4. The audio data is streamed with the adjusted volume level
5. The process is repeated for each audio file, creating a consistent listening experience
