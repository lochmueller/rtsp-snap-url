# rtsp-snap-url

Tiny Docker container that exposes RTSP camera streams as cached HTTP JPEG snapshots.

Inspired by [dewgenenny/docker_rtsp_grab](https://github.com/dewgenenny/docker_rtsp_grab), but
designed for many cameras at once via a config file, with per-camera caching TTLs so a stream
is never hit more often than necessary.

## How it works

- A small Go binary (`rtsp-snap`) acts as both the HTTP server and the snapshot dispatcher.
- A request to `http://host:8080/<key>.jpg` looks up `<key>` in the config:
  - If the cached snapshot for that key is younger than its TTL, it is served directly.
  - Otherwise `ffmpeg` grabs a single frame from the RTSP stream, the cache is replaced atomically, and the new image is served.
- A per-key mutex serializes concurrent requests for the same camera, so only one `ffmpeg` ever runs at a time per stream.
- The config file is reloaded automatically when its mtime changes — no restart needed for URL or TTL changes, or for adding/removing keys.

## Configuration

Mount a YAML file at `/etc/rtsp-snap/config.yaml`:

```yaml
default_ttl: 30
default_transport: tcp

streams:
  front_door:
    url: rtsp://user:pass@192.168.1.10:554/stream1
    ttl: 30
  backyard:
    url: rtsp://192.168.1.11:554/h264
  garage:
    url: rtsp://192.168.1.12/h264
    ttl: 120
    transport: udp
  driveway:
    url: rtsp://192.168.1.13/h264
    interval: 60
    archive: 100
```

Per-stream fields:

| Field       | Default | Meaning                                                                                                       |
| ----------- | ------- | ------------------------------------------------------------------------------------------------------------- |
| `url`       | —       | RTSP source URL (required).                                                                                   |
| `ttl`       | `30`    | Cache validity in seconds. Repeated requests inside the window return the cached frame.                       |
| `transport` | `tcp`   | RTSP transport: `tcp` (more reliable) or `udp`.                                                               |
| `interval`  | `0`     | Auto-snapshot interval in seconds. The scheduler refreshes the cache every `interval` seconds, even without HTTP traffic. `0` disables. |
| `archive`   | `0`     | How many previous snapshots to keep. `0` = no archive, `-1` = unlimited, `N` = keep the last `N`.             |

- The key (e.g. `front_door`) becomes the URL path: `/front_door.jpg`.
- Keys must match `[A-Za-z0-9_-]+`.

### Archive layout

Archived snapshots are written to `{RTSP_CACHE_DIR}/archive/<key>/<UTC-timestamp>.jpg` (timestamp format `2006-01-02_15-04-05.000Z`). The timestamp reflects when that frame was originally grabbed. Files are rotated atomically — the previous cache file is moved into the archive, then the fresh capture replaces the cache. Mount the cache directory as a Docker volume if you want the archive to survive container restarts.

## Run

Pre-built images are published to GitHub Container Registry on every push to `main`:

```bash
docker pull ghcr.io/lochmueller/rtsp-snap-url:latest
```

Or build locally:

```bash
docker build -t rtsp-snap-url .

docker run -d \
  --name rtsp-snap \
  -p 8080:8080 \
  -v "$PWD/config.yaml:/etc/rtsp-snap/config.yaml:ro" \
  rtsp-snap-url
```

Then browse to `http://localhost:8080/front_door.jpg`.

Or with compose (copy `docker-compose.example.yml` to `docker-compose.yml`):

```bash
docker compose up -d
```

## Environment variables / flags

Every env var also has an equivalent CLI flag.

| Variable              | Flag                 | Default                      | Purpose                          |
| --------------------- | -------------------- | ---------------------------- | -------------------------------- |
| `PORT`                | `--port`             | `8080`                       | HTTP listen port                 |
| `BIND`                | `--bind`             | `0.0.0.0`                    | Bind address                     |
| `RTSP_CONFIG`         | `--config`           | `/etc/rtsp-snap/config.yaml` | Path to YAML config              |
| `RTSP_CACHE_DIR`      | `--cache`            | `/var/cache/rtsp-snap`       | Directory for cached snapshots   |
| `RTSP_FFMPEG_TIMEOUT` | `--ffmpeg-timeout`   | `15`                         | Per-grab ffmpeg timeout (sec)    |

## Error responses

| Status                | Cause                                                                     |
| --------------------- | ------------------------------------------------------------------------- |
| `404 Not Found`       | Key not present in config, or path doesn't match `<key>.jpg`.             |
| `502 Bad Gateway`     | ffmpeg failed (camera offline, bad URL). Stale frame served if available. |
| `504 Gateway Timeout` | ffmpeg exceeded `RTSP_FFMPEG_TIMEOUT`.                                    |
| `500 …`               | Config missing or unreadable.                                             |

## Endpoints

| Path           | Purpose                                                                |
| -------------- | ---------------------------------------------------------------------- |
| `/`            | HTML index — grid of configured streams with lazy-loaded thumbnails.   |
| `/<key>.jpg`   | Snapshot for the given stream key (cached, refreshed per TTL).         |
| `/healthz`     | Liveness probe — returns `200` if the config is readable, `503` else.  |

## License

[MIT](LICENSE) — see `LICENSE` file.

## Build without Docker

```bash
go build -trimpath -ldflags='-s -w' -o rtsp-snap .
./rtsp-snap --config ./config.yaml --cache /tmp/rtsp-cache
```

Requires `ffmpeg` on `$PATH`.

## Image size

The Dockerfile uses three build stages:

1. **ffmpeg-build** — compiles a trimmed `ffmpeg` (~10 MB) with only the demuxers/decoders/encoders needed for `RTSP → H.264/H.265/MJPEG → JPEG`. No documentation, no extra protocols, no audio codecs.
2. **go-build** — compiles the Go binary statically (~6 MB).
3. **final** — `alpine:3.19` + the two binaries. ~25 MB total.

The trimmed ffmpeg supports H.264 (most cameras), H.265 (newer cameras), and MJPEG (cheap cameras). If your camera uses something exotic (MPEG-4, VP8/9), enable the corresponding decoder/parser in the `./configure` line in the Dockerfile.

The first build takes 5–10 minutes (ffmpeg compile); subsequent builds reuse the cached stage.
