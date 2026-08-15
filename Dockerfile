FROM python:3.10-slim AS build

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
COPY httpaceproxycpp ./httpaceproxycpp

RUN cmake -S /src/httpaceproxycpp -B /build -DCMAKE_BUILD_TYPE=Release -DHTTPACEPROXYCPP_BUILD_TESTS=OFF && \
    cmake --build /build -j "$(nproc)"

FROM python:3.10-slim

WORKDIR /app

RUN apt-get update && apt-get install -y ffmpeg libcurl4 zlib1g && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

COPY --from=build /build/httpaceproxycpp /app/httpaceproxycpp_bin

# Expose port for the Flask app (HDHomeRun uses 5004 by default for streaming API)
EXPOSE 5004

CMD ["python", "app.py"]
