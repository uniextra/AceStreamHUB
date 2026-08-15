# HTTPAceProxyCPP

Native C++20 implementation of HTTPAceProxy.

The binary and Docker image are named `httpaceproxycpp`. This variant does not
start AceStream/AceServe by itself; it connects to an existing engine through
`ACESTREAM_HOST`, `ACESTREAM_API_PORT`, and `ACESTREAM_HTTP_PORT`.

## Build Locally

```bash
cmake -S httpaceproxycpp -B httpaceproxycpp/build -DHTTPACEPROXYCPP_BUILD_TESTS=ON
cmake --build httpaceproxycpp/build -j
ctest --test-dir httpaceproxycpp/build --output-on-failure
```

On this macOS host, the Command Line Tools installation is missing libc++ headers,
so the verified local build used Homebrew LLVM:

```bash
cmake -S httpaceproxycpp -B httpaceproxycpp/build-llvm-tests \
  -DHTTPACEPROXYCPP_BUILD_TESTS=ON \
  -DCMAKE_CXX_COMPILER=/opt/homebrew/opt/llvm/bin/clang++
cmake --build httpaceproxycpp/build-llvm-tests -j
ctest --test-dir httpaceproxycpp/build-llvm-tests --output-on-failure
```

## Run Without AceStream For UI Checks

This starts only the C++ proxy with dashboard plugins enabled:

```bash
ENABLED_PLUGINS=stat,statplugin ACEPROXY_PORT=18888 \
  ./httpaceproxycpp/build-llvm-tests/httpaceproxycpp
```

Then check:

```bash
curl http://127.0.0.1:18888/stat?action=get_status
curl http://127.0.0.1:18888/statplugin?action=get_plugins
```

## Docker Build

Use the repository root as build context:

```bash
docker build -f httpaceproxycpp/Dockerfile -t httpaceproxycpp:latest .
```

Run against an existing AceStream/AceServe engine:

```bash
docker run --rm \
  --name httpaceproxycpp \
  -p 8888:8888 \
  -e ACESTREAM_HOST=your_acestream_host \
  -e ACESTREAM_API_PORT=62062 \
  -e ACESTREAM_HTTP_PORT=6878 \
  httpaceproxycpp:latest
```

Or use the compose file that only starts the proxy:

```bash
docker compose -f httpaceproxycpp/docker-compose-httpaceproxycpp.yml up -d
```

## Implemented Surface

- Core streaming routes: `/content_id`, `/pid`, `/infohash`, `/url`,
  `/direct_url`, `/data`, `/efile_url`.
- AceStream API protocol: `HELLOBG`, `READY`, `LOADASYNC`, `START`, `STATUS`,
  `STOP`, `SHUTDOWN`, `LIVESEEK`, `SETOPTIONS`.
- Broadcast sharing: one AceStream broadcast per `infohash`, many HTTP clients.
- Limits: `MAX_CONNECTIONS` and `MAX_CONCURRENT_CHANNELS`.
- Plugin endpoints: `newera`, `elcano`, `acepl`, `af1c1onados`, `aio`,
  `stat`, `statplugin`.
- Static dashboard assets from `httpaceproxycpp/http/`.

## Current Verification

Verified locally without starting AceStream/AceServe:

- CMake configure and compile.
- Unit tests for hashing, URL parsing, JSON parsing, and M3U generation.
- Live HTTP checks for `/stat?action=get_status` and
  `/statplugin?action=get_plugins` with only dashboard plugins enabled.

Streaming verification against a real AceStream/AceServe engine is intentionally
left for a separate machine to avoid local P2P traffic.
