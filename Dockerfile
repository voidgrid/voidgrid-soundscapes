# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/voidgrid-soundscapes ./cmd/voidgrid-soundscapes

FROM alpine:3.23
# ffmpeg decodes every layer and encodes the MP3 stream.
RUN apk add --no-cache ffmpeg \
 && addgroup -g 1000 soundscapes \
 && adduser -D -u 1000 -G soundscapes -h /home/soundscapes -s /sbin/nologin soundscapes \
 && mkdir -p /soundscapes/audio /soundscapes/config \
 && chown 1000:1000 /soundscapes /soundscapes/audio /soundscapes/config
COPY --from=build /out/voidgrid-soundscapes /usr/local/bin/voidgrid-soundscapes

ENV HOME=/home/soundscapes \
    VOIDGRID_SOUNDSCAPES_AUDIO_DIR=/soundscapes/audio \
    VOIDGRID_SOUNDSCAPES_CONFIG_DIR=/soundscapes/config
USER 1000:1000
VOLUME ["/soundscapes/audio", "/soundscapes/config"]
EXPOSE 8007
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["voidgrid-soundscapes", "healthcheck"]
ENTRYPOINT ["voidgrid-soundscapes"]
