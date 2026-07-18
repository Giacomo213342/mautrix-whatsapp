FROM golang:1-alpine3.24 AS builder

RUN apk add --no-cache git ca-certificates build-base su-exec olm-dev opus-dev opusfile-dev

WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . /build
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    ./build.sh

FROM alpine:3.24

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache ffmpeg su-exec ca-certificates olm opus opusfile bash jq curl yq-go lottieconverter

COPY --from=builder /build/mautrix-whatsapp /usr/bin/mautrix-whatsapp
COPY --from=builder /build/docker-run.sh /docker-run.sh
VOLUME /data

CMD ["/docker-run.sh"]
