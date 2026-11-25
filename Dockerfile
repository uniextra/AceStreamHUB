FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /acestreamhub ./cmd/acestreamhub

FROM alpine:3.21

WORKDIR /app

RUN apk add --no-cache ffmpeg ca-certificates tzdata && \
    addgroup -g 1000 appuser && \
    adduser -u 1000 -G appuser -h /app -D -s /sbin/nologin appuser

COPY --from=builder /acestreamhub /app/acestreamhub

RUN mkdir -p /app/config && \
    chown -R appuser:appuser /app

USER appuser

EXPOSE 5004

CMD ["/app/acestreamhub"]
