# SpotiFLAC HTTP Server Mode

SpotiFLAC can run as a headless HTTP server for integration with Lidarr or other automation tools.

## Quick Start

### Local Development

```bash
# Build the binary
cd SpotiFLAC
pnpm install --prefix frontend && pnpm run build --prefix frontend  # Build frontend first
go build -o spotiflac-server .

# Run the server
./spotiflac-server --server --port 8787 --output /path/to/downloads
```

### Docker

```bash
# Build and run with docker-compose (from project root)
docker-compose up -d spotiflac

# Or build standalone
cd SpotiFLAC
docker build -f Dockerfile.server -t spotiflac-server .
docker run -d -p 8787:8787 -v /path/to/downloads:/downloads/spotiflac spotiflac-server
```

## CLI Options

| Flag | Default | Description |
|------|---------|-------------|
| `--server` | false | Enable HTTP server mode (required) |
| `--port` | 8787 | HTTP server port |
| `--output` | /data/downloads/spotiflac | Download output directory |
| `--service` | amazon | Default music service: amazon, tidal, qobuz |
| `--quality` | LOSSLESS | Default audio quality |
| `--embed-lyrics` | true | Embed lyrics in audio files |
| `--embed-cover` | true | Embed high quality cover art |
| `--filename-format` | title-artist | Filename format pattern |

## API Endpoints

### GET /api/health

Health check endpoint.

**Response:**
```json
{
  "status": "ok",
  "version": "7.1.2",
  "service": "amazon",
  "output_dir": "/downloads/spotiflac"
}
```

### POST /api/import

Import a Spotify URL (track, album, or playlist).

**Request:**
```json
{
  "url": "https://open.spotify.com/playlist/0UyFoxjRgUaxCwI0FyKUsx",
  "service": "amazon",
  "quality": "LOSSLESS",
  "embed_lyrics": true,
  "embed_cover": true
}
```

**Response:**
```json
{
  "success": true,
  "job_id": "job-1234567890",
  "message": "playlist queued for import"
}
```

### GET /api/queue

Get the current download queue status.

**Response:**
```json
{
  "items": [
    {
      "id": "track-123",
      "track_name": "Song Title",
      "artist_name": "Artist Name",
      "status": "downloading",
      "progress": 45.5,
      "speed": 1.2
    }
  ],
  "total": 15,
  "completed": 3,
  "failed": 0,
  "queued": 12,
  "skipped": 0
}
```

### DELETE /api/queue

Clear the download queue.

### GET /api/progress

Get current download progress.

**Response:**
```json
{
  "is_downloading": true,
  "current_speed_mbps": 1.5,
  "mb_downloaded": 25.3
}
```

### GET /api/history

Get download history.

### DELETE /api/history

Clear download history.

### GET /api/jobs

Get all import jobs.

## Lidarr Integration

### Configure Lidarr Blackhole Client

1. Go to Lidarr → Settings → Download Clients
2. Click "+" → Select "Usenet Blackhole" (or Torrent Blackhole)
3. Configure:
   - **Name:** SpotiFLAC
   - **Watch Folder:** `/downloads/spotiflac` (must match SpotiFLAC output)

### Output Directory Structure

SpotiFLAC saves files in a Lidarr-compatible format:

```
/downloads/spotiflac/
└── Artist Name - Album Name (2024)/
    ├── 01 - Track Title.flac
    ├── 02 - Track Title.flac
    └── ...
```

For playlists and albums, tracks are organized in artist-album folders.
For single tracks, files are saved directly in the output directory.

## Example Usage

```bash
# Import a single track
curl -X POST http://localhost:8787/api/import \
  -H "Content-Type: application/json" \
  -d '{"url": "https://open.spotify.com/track/4iV5W9uYEdYUVa79Axb7Rh"}'

# Import an album
curl -X POST http://localhost:8787/api/import \
  -H "Content-Type: application/json" \
  -d '{"url": "https://open.spotify.com/album/4LH4d3cOWNNsVw41Gqt2kv"}'

# Import a playlist
curl -X POST http://localhost:8787/api/import \
  -H "Content-Type: application/json" \
  -d '{"url": "https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M"}'

# Check download queue
curl http://localhost:8787/api/queue

# Check server health
curl http://localhost:8787/api/health
```
