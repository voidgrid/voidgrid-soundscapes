# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Format, vet and tests gate the image: a failure here fails the build.
RUN test -z "$(gofmt -l cmd internal)" && go vet ./... && go test ./...
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/voidgrid-soundscapes ./cmd/voidgrid-soundscapes

# Alpine because ffmpeg is needed at runtime (it decodes every layer and encodes the stream).
# The Go binary is static and embeds the web UI, so nothing else is required.
FROM alpine:3.23
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
