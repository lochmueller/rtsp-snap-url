# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.19
ARG FFMPEG_VERSION=6.1.1
ARG GO_VERSION=1.22

# ---- Stage 1: trimmed ffmpeg ----
# Builds ffmpeg from source with only what we need for RTSP -> single JPEG:
# rtsp/sdp/rtp demuxers, h264/hevc/mjpeg decoders, mjpeg encoder, image2 muxer.
# Result: ~10 MB binary instead of ~38 MB from `apk add ffmpeg`.
FROM alpine:${ALPINE_VERSION} AS ffmpeg-build
ARG FFMPEG_VERSION
RUN apk add --no-cache build-base nasm yasm pkgconfig curl tar xz

WORKDIR /build
RUN curl -fsSL "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" | tar -xJ
WORKDIR /build/ffmpeg-${FFMPEG_VERSION}

RUN ./configure \
        --prefix=/ffmpeg \
        --disable-everything \
        --disable-doc \
        --disable-debug \
        --disable-htmlpages --disable-manpages --disable-podpages --disable-txtpages \
        --disable-autodetect \
        --disable-zlib --disable-bzlib --disable-iconv --disable-lzma \
        --disable-ffplay --disable-ffprobe \
        --enable-small \
        --enable-protocol=file,tcp,udp,rtp \
        --enable-demuxer=rtsp,sdp,rtp \
        --enable-decoder=h264,hevc,mjpeg \
        --enable-parser=h264,hevc,mjpeg \
        --enable-encoder=mjpeg \
        --enable-muxer=image2 \
        --enable-filter=scale,format \
        --enable-swscale \
        --extra-cflags="-Os" \
 && make -j"$(nproc)" \
 && make install \
 && strip /ffmpeg/bin/ffmpeg

# ---- Stage 2: Go binary ----
FROM golang:${GO_VERSION}-alpine AS go-build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN go mod tidy \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /rtsp-snap .

# ---- Stage 3: final ----
FROM alpine:${ALPINE_VERSION}
RUN mkdir -p /etc/rtsp-snap /var/cache/rtsp-snap

COPY --from=ffmpeg-build /ffmpeg/bin/ffmpeg /usr/local/bin/ffmpeg
COPY --from=go-build     /rtsp-snap         /usr/local/bin/rtsp-snap
COPY config.example.yaml /etc/rtsp-snap/config.yaml

ENV PORT=8080 \
    BIND=0.0.0.0 \
    RTSP_CONFIG=/etc/rtsp-snap/config.yaml \
    RTSP_CACHE_DIR=/var/cache/rtsp-snap \
    RTSP_FFMPEG_TIMEOUT=15

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/rtsp-snap"]
