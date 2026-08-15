<p align="center">
  <img src="static/img/logo.jpg" alt="AceStream Hub Logo" width="150" height="150" style="border-radius: 20px;">
</p>

# AceStream Hub (AceScrapper Proxy)

<p align="center">
  <a href="https://github.com/uniextra/AceScrapper/actions/workflows/docker-publish.yml"><img src="https://github.com/uniextra/AceScrapper/actions/workflows/docker-publish.yml/badge.svg" alt="Release Pipeline"></a>
  <a href="https://hub.docker.com/r/uniextra/acestreamhub"><img src="https://img.shields.io/docker/pulls/uniextra/acestreamhub.svg" alt="Docker Pulls"></a>
  <a href="https://ko-fi.com/uniextra"><img src="https://img.shields.io/badge/Ko--fi-Buy%20me%20a%20coffee-F16061?logo=ko-fi&logoColor=white" alt="Ko-fi"></a>
</p>

AceScrapper is a powerful, automated IPTV proxy and M3U generator designed to seamlessly bridge AceStream broadcasts with standard IPTV players (like IPTV Smarters Pro, VLC, etc.). It supports multiple simultaneous channels and features a built-in HDHomeRun emulator for easy integration with your local media servers.

It provides a modern, Apple-inspired Web Dashboard for managing your sources and mappings.

## Features

- **AceStream to HTTP Proxying**: Uses the embedded `acexy` engine to convert P2P AceStream broadcasts into standard HTTP video streams on the fly.
- **Dynamic M3U & EPG XML Generation**: Automatically generates a `/playlist.m3u` and a separate `/epg.xml` file, ready to be ingested by any IPTV player.
- **HDHomeRun Emulator**: Built-in network tuner emulator (`/discover.json`, `/lineup.json`) to seamlessly integrate with Plex, Emby, and Jellyfin as a Live TV Tuner.
- **Automated Source Scraping**: Built-in background task scheduler that fetches, parses, and merges multiple M3U or HTML scraping sources at custom intervals.
- **Manual Channels & ID Management**: Easily add custom AceStream IDs manually or bind them to existing channels through the UI.
- **Smart EPG Mapping & Dummy Events**: Intelligently fuzzy-matches your AceStream channels to official XMLTV EPG IDs. Injects "blank" dummy events for unmapped channels so they don't break your player's guide.
- **Background Health Checks**: Scans channels and their adjacent streams automatically in the background to ensure high availability.
- **Auto-Discard Unhealthy Streams**: Detects failing streams and automatically discards them based on customizable timeout settings.
- **Web Dashboard**: A sleek, frosted-glass UI (following Apple's Human Interface Guidelines) to monitor real-time active streams, manage sources, and map channels.

## Requirements
- **Docker & Docker Compose**: The entire stack is containerized for zero-hassle deployment.

## Installation

### Option A: Using Pre-built Docker Image (Recommended)

1. Create a `docker-compose.yml` file anywhere on your system:
   ```yaml
   services:
     acestream-engine:
       image: jopsis/aceserve:latest
       container_name: acestream-engine
       restart: unless-stopped
       ports:
         - "6878:6878"
         - "8621:8621"
       volumes:
         - ./config:/config
       command: >
         /bin/sh -c 'args=$$(cat /config/engine_args.txt 2>/dev/null || echo ""); python main.py --bind-all --live-cache-type memory --live-mem-cache-size 104857600 --disable-sentry --log-stdout --disable-upnp $$args'
   
     acexy:
       image: ghcr.io/javinator9889/acexy:0.2.2
       container_name: acexy
       restart: unless-stopped
       ports:
         - "8080:8080"
       environment:
         ACEXY_LISTEN_ADDR: ":8080"
         ACEXY_SCHEME: "http"
         ACEXY_HOST: "acestream-engine"
         ACEXY_PORT: "6878"
       depends_on:
         - acestream-engine
       
     proxy-app:
       image: uniextra/acestreamhub:latest
       container_name: acestream-hdhr-proxy
       restart: unless-stopped
       ports:
         - "${WEB_PORT:-5004}:${WEB_PORT:-5004}"
       environment:
         - ACESTREAM_URL=http://acexy:8080
         - DATA_DIR=/app/config
         - WEB_PORT=${WEB_PORT:-5004}
         # Opcional: Descomenta temporalmente la siguiente línea si necesitas resetear tu contraseña
         # - RESET_PASSWORD=true
       volumes:
         - ./config:/app/config
       depends_on:
         - acestream-engine
         - acexy
   ```
2. Start the stack:
   ```bash
   docker-compose up -d
   ```

### Option B: Build from Source

1. Clone the repository:
   ```bash
   git clone https://github.com/uniextra/AceStreamHUB.git
   cd AceStreamHUB
   ```
2. Start the Docker containers:
   ```bash
   docker-compose up -d --build
   ```

## Usage

1. **Access the UI**: Open the web dashboard at `http://localhost:5004`. On your very first visit, you will be prompted to create an access password. If you ever forget it, you can reset it by setting `RESET_PASSWORD=true` in your Docker environment variables.
2. **Add a Source**: Navigate to **Sources** and add your M3U URL (or website URL to scrape). The engine will download and parse the channels.
3. **Add EPG**: Navigate to **Settings** and paste your XMLTV URL. You can also toggle the **HDHomeRun Emulator** here.
4. **Map Channels**: Navigate to **Channels & EPG**. Map your AceStream channels to the official EPG guide, run speed tests to find the best peer-to-peer source, and set your primary links.
5. **Load into Player**: Point your IPTV Player to `http://<your-server-ip>:5004/playlist.m3u` (and `/epg.xml`) or let Plex auto-discover the HDHomeRun Tuner.

## Privacy Note
The `config.json` file generated locally contains your private EPG and Source URLs. It is safely ignored via `.gitignore` and will never be uploaded to this repository.

## Credits & Acknowledgements
- `jopsis/docker-acestream-aceserve` for the Dockerized AceStream Engine.
- `javinator9889/acexy` for the AceStream to HTTP proxy engine.
