<p align="center">
  <img src="static/img/logo.jpg" alt="AceStream Hub Logo" width="150" height="150" style="border-radius: 20px;">
</p>

# AceStream Hub (AceScrapper Proxy)

<p align="center">
  <a href="https://github.com/uniextra/AceStreamHUB/actions/workflows/docker-publish.yml"><img src="https://github.com/uniextra/AceStreamHUB/actions/workflows/docker-publish.yml/badge.svg" alt="Release Pipeline"></a>
  <a href="https://hub.docker.com/r/uniextra/acestreamhub"><img src="https://img.shields.io/docker/pulls/uniextra/acestreamhub.svg" alt="Docker Pulls"></a>
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-blue.svg" alt="License: MIT"></a>
  <a href="https://ko-fi.com/uniextra"><img src="https://img.shields.io/badge/Ko--fi-Buy%20me%20a%20coffee-F16061?logo=ko-fi&logoColor=white" alt="Ko-fi"></a>
</p>

> [!WARNING]
> **No longer maintained:** This project is no longer actively maintained. We recommend using **[Pipepito/acestream-scraper](https://github.com/Pipepito/acestream-scraper)** instead.

AceStream Hub is a fast, lightweight IPTV proxy and M3U/XMLTV orchestrator rewritten in **Go** to connect **directly** to `acestream-engine` and bridge AceStream P2P broadcasts to standard IPTV players (VLC, TiviMate, Kodi, IPTV Smarters Pro) and media servers via a built-in HDHomeRun tuner emulator.

It ships as a **single static zero-CGO binary** (`embed.FS` templates & assets + pure-Go SQLite) inside a hardened non-root `alpine:3.21` container (~15–25 MB RAM footprint), featuring a built-in Go stream multiplexer (removing the need for `acexy`) and a modern frosted-glass Web Dashboard.

## Branches

- **`go`** *(active)*: Pure-Go single-binary rewrite connecting directly to `acestream-engine` with a built-in RAM ring-buffer multiplexer, MPEG-TS aligned Hot Failover, and direct `command_url?method=stop` lifecycle management. Drop-in compatible with existing `config.json` (including Werkzeug PBKDF2/scrypt password hashes) and `scans.db`.
- **`legacy`**: Original Python/Flask implementation using `acexy` (kept for reference).

## Features

- **Direct AceStream Engine Integration (No `acexy` Needed)**: Connects directly to `acestream-engine` (`format=json`), reducing the stack to just 2 containers, cutting memory usage, and capturing session `command_url`/`stat_url` for instant teardown and live peer stats. *(If you still prefer an external HTTP proxy, set `ACESTREAM_URL`.)*
- **Go Single-Binary Architecture**: Zero-CGO static binary with embedded HTML/assets, pure-Go SQLite (`modernc.org/sqlite`), and a minimal non-root Alpine Docker image.
- **RAM Ring-Buffer Multiplexer**: Shares a single upstream AceStream connection across multiple simultaneous IPTV viewers using a volatile ~4 MB RAM ring buffer (zero SSD/disk wear), giving late-joining clients instant playback start without stalling healthy players.
- **In-Stream Hot Failover**: If an active AceStream source drops mid-broadcast, the hub keeps the player's HTTP socket open, penalizes the failed ID in SQLite, stops the dead session immediately, dials the next quality-ranked Ace ID, aligns to the MPEG-TS `0x47` sync byte, and resumes playback seamlessly.
- **On-Demand Engine Lifecycle**: Immediately stops test swarms after channel scans (`?method=stop`), tears down the engine session as soon as the last viewer disconnects, and runs a background idle monitor (`engine_idle_timeout`) to reclaim any lingering sessions automatically — plus a one-click manual purge button in Settings.
- **Real-Time Telemetry Dashboard**: Live monitor displaying active channels, current Ace ID, live bitrate (Mbps), P2P peer count, data transferred, uptime, and connected player IPs/agents.
- **Dynamic M3U & EPG XML Generation**: Automatically generates `/playlist.m3u` and `/epg.xml`, ready to be ingested by any IPTV player.
- **HDHomeRun Emulator**: Built-in network tuner emulator (`/discover.json`, `/lineup.json`) for native integration with Plex, Emby, and Jellyfin.
- **Automated Source Scraping & Quality Ranking**: Background scheduler that fetches and merges M3U, JSON, HTML scrape, and XMLTV EPG feeds; tests candidate Ace IDs via engine stats + `ffprobe` and ranks backups by resolution and peer count.
- **Smart EPG Mapping & Dummy Events**: Fuzzy-matches channels to XMLTV EPG IDs and injects dummy events for unmapped channels so player guides stay intact.
- **Auto-Discard Unhealthy Streams**: Automatically skips sources with 3+ consecutive failures during tuning and failover.

## Requirements
- **Docker & Docker Compose** (recommended), or **Go 1.26+** + `ffmpeg`/`ffprobe` for local builds.

## Installation

### Option A: Using Pre-built Docker Image (Recommended)

1. Create a `docker-compose.yml` file anywhere on your system:
   ```yaml
   services:
     acestream-engine:
       image: vstavrinov/acestream-engine:latest
       container_name: acestream-engine
       restart: unless-stopped
       shm_size: "1gb"
       ports:
         - "6878:6878"
         - "8621:8621"
       volumes:
         - ./config:/config
       command: >
         /bin/bash -c "args=$$(cat /config/engine_args.txt 2>/dev/null || echo ''); mkdir -p /dev/shm/.ACEStream; ./start-engine --client-console --bind-all --live-cache-type memory $$args"

     proxy-app:
       image: uniextra/acestreamhub:latest
       container_name: acestream-hdhr-proxy
       restart: unless-stopped
       ports:
         - "${WEB_PORT:-5004}:${WEB_PORT:-5004}"
       environment:
         - ENGINE_URL=http://acestream-engine:6878
         - DATA_DIR=/app/config
         - WEB_PORT=${WEB_PORT:-5004}
         # Optional: Temporarily uncomment to reset your admin password on next startup
         # - RESET_PASSWORD=true
       volumes:
         - ./config:/app/config
       depends_on:
         - acestream-engine
   ```
2. Start the stack:
   ```bash
   docker-compose up -d
   ```

### VPN / Gluetun Configuration (Optional)
If you want to route all AceStream traffic through a VPN container like Gluetun, you can bind both services to the `gluetun` network space. Update your `docker-compose.yml` like this:

```yaml
services:
  acestream-engine:
    image: vstavrinov/acestream-engine:latest
    container_name: acestream-engine
    restart: unless-stopped
    shm_size: "1gb"
    network_mode: "service:gluetun"
    security_opt:
      - no-new-privileges:true
    volumes:
      - ./config:/config
    command: >
      /bin/bash -c "args=$$(cat /config/engine_args.txt 2>/dev/null || echo ''); mkdir -p /dev/shm/.ACEStream; ./start-engine --client-console --bind-all --live-cache-type memory $$args"
    depends_on:
      gluetun:
        condition: service_healthy

  proxy-app:
    image: uniextra/acestreamhub:latest
    container_name: acestream-hdhr-proxy
    restart: unless-stopped
    network_mode: "service:gluetun"
    security_opt:
      - no-new-privileges:true
    environment:
      - ENGINE_URL=http://127.0.0.1:6878
      - DATA_DIR=/app/config
      - WEB_PORT=${WEB_PORT:-5004}
    volumes:
      - ./config:/app/config
    depends_on:
      gluetun:
        condition: service_healthy
```

### Option B: Build from Source

1. Clone the repository:
   ```bash
   git clone -b go https://github.com/uniextra/AceStreamHUB.git
   cd AceStreamHUB
   ```
2. Start the Docker containers:
   ```bash
   docker-compose up -d --build
   ```

### Local Go Development

```bash
go test -v ./...
go build -o acestreamhub ./cmd/acestreamhub
```

## Usage

1. **Access the UI**: Open the web dashboard at `http://localhost:5004`. On your very first visit, you will be prompted to create an access password. If you ever forget it, set `RESET_PASSWORD=true` temporarily in your Docker environment variables.
2. **Add Sources**: Navigate to **Sources** and add your M3U, JSON, scrape, or XMLTV EPG URLs.
3. **Configure Engine & Failover**: Navigate to **Settings** to tune stream timeouts, Hot Failover, on-demand engine idle timeout, live buffer, or toggle the **HDHomeRun Emulator**.
4. **Map & Test Channels**: Navigate to **Channels** to map channels to EPG entries, run speed/resolution probes, reorder channels, or pin primary Ace IDs.
5. **Load into Player**: Point your IPTV Player to `http://<your-server-ip>:5004/playlist.m3u` and `http://<your-server-ip>:5004/epg.xml` (or let Plex/Jellyfin auto-discover the HDHomeRun Tuner).

## Privacy Note
The `config.json` file generated locally contains your private EPG and Source URLs. It is safely ignored via `.gitignore` and will never be uploaded to this repository.

## Credits & Acknowledgements
- `vstavrinov/acestream-engine` for the Dockerized AceStream Engine.
- `javinator9889/acexy` (used in the `legacy` Python stack and inspiration for the built-in Go stream multiplexer).

## License
Distributed under the MIT License. See [LICENSE](LICENSE) for more information.
