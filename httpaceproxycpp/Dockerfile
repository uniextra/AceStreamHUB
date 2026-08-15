FROM ubuntu:24.04 AS build

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      build-essential \
      ca-certificates \
      cmake \
      curl \
      libcurl4-openssl-dev \
      zlib1g-dev && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY httpaceproxycpp/CMakeLists.txt ./httpaceproxycpp/CMakeLists.txt
COPY httpaceproxycpp/include ./httpaceproxycpp/include
COPY httpaceproxycpp/src ./httpaceproxycpp/src
COPY httpaceproxycpp/tests ./httpaceproxycpp/tests

RUN cmake -S /src/httpaceproxycpp -B /build -DCMAKE_BUILD_TYPE=Release -DHTTPACEPROXYCPP_BUILD_TESTS=ON && \
    cmake --build /build -j "$(nproc)" && \
    ctest --test-dir /build --output-on-failure

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      libcurl4t64 \
      zlib1g && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /build/httpaceproxycpp /app/httpaceproxycpp
COPY httpaceproxycpp/http /app/http

ENV ACEPROXY_HOST=0.0.0.0
ENV ACEPROXY_PORT=8888
ENV ACESTREAM_HOST=127.0.0.1
ENV ACESTREAM_API_PORT=62062
ENV ACESTREAM_HTTP_PORT=6878
ENV MAX_CONNECTIONS=10
ENV MAX_CONCURRENT_CHANNELS=5
ENV ENABLED_PLUGINS=newera,elcano,acepl,af1c1onados,aio,stat,statplugin
ENV AIO_PLUGINS=newera,elcano,acepl,af1c1onados

EXPOSE 8888

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
  CMD curl -f http://127.0.0.1:${ACEPROXY_PORT}/stat || exit 1

ENTRYPOINT ["/app/httpaceproxycpp"]
