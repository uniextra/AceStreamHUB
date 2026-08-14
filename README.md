<p align="center">
  <img src="static/img/logo.jpg" alt="AceStream Hub Logo" width="150" height="150" style="border-radius: 20px;">
</p>

# AceStream Hub (AceScrapper Proxy)

AceScrapper is a powerful, automated IPTV proxy and M3U generator designed to seamlessly bridge AceStream broadcasts with standard IPTV players (like IPTV Smarters Pro, VLC, etc.).

It features a premium, Apple-inspired Web Dashboard for managing your sources and mappings.

## Features

- **AceStream to HTTP Proxying**: Uses the embedded `acexy` engine to convert P2P AceStream broadcasts into standard HTTP video streams on the fly.
- **Dynamic M3U Generation**: Automatically generates a `/playlist.m3u` file containing your parsed channels, ready to be ingested by any IPTV player.
- **Automated Source Scraping**: Built-in background task scheduler (`APScheduler`) that can fetch, parse, and merge multiple M3U or HTML scraping sources at custom intervals (1h, 6h, 12h, 24h).
- **IPFS Gateway Resolution**: Automatically resolves Web3 `.inbrowser.link` URLs via `dweb.link` for decentralized IPTV lists.
- **Smart EPG Mapping**: Input your favorite XMLTV EPG URL. The engine will intelligently fuzzy-match and map your AceStream channel names to official EPG IDs and inject them into your final M3U playlist.
- **Premium Web Dashboard**: A sleek, frosted-glass UI (following Apple's Human Interface Guidelines) to monitor active streams, manage sources, and map channels.

## Requirements
- **Docker & Docker Compose**: The entire stack is containerized for zero-hassle deployment.

## Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/uniextra/AceScrapper.git
   cd AceScrapper
   ```
2. Start the Docker containers:
   ```bash
   docker-compose up -d --build
   ```
3. Open the web dashboard in your browser:
   ```
   http://localhost:5004
   ```

## Usage

1. **Add a Source**: In the Dashboard, navigate to **Sources** and add your M3U URL (or website URL to scrape). The engine will download and parse the channels.
2. **Add EPG**: Navigate to **Settings** and paste your XMLTV URL.
3. **Map Channels**: Navigate to **Channels & EPG**. The engine will automatically attempt to map your AceStream channels to the official EPG guide. You can manually adjust any mappings here.
4. **Load into Player**: Point your IPTV Player to `http://<your-server-ip>:5004/playlist.m3u` and enjoy!

## Privacy Note
The `config.json` file generated locally contains your private EPG and Source URLs. It is safely ignored via `.gitignore` and will never be uploaded to this repository.

## Credits & Acknowledgements
- `vstavrinov/acestream-engine` for the Dockerized AceStream Engine.
- `javinator9889/acexy` for the AceStream to HTTP proxy engine.
