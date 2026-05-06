# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS builder
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y --no-install-recommends \
        libfdk-aac-dev \
        libavcodec-dev libavutil-dev libswscale-dev \
        && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /app/homekit-rtsp-proxy ./cmd/homekit-rtsp-proxy/

FROM debian:bookworm-slim
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources && \
    apt-get update && apt-get install -y --no-install-recommends \
        libfdk-aac2 \
        libavcodec59 libavutil57 libswscale6 \
        ca-certificates \
        && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/homekit-rtsp-proxy /app/homekit-rtsp-proxy
WORKDIR /app/data
CMD ["/app/homekit-rtsp-proxy", "-config", "config.yaml"]
