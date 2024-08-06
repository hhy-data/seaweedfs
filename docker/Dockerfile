FROM --platform=${BUILDPLATFORM} vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm-dev/golang:1.26.2-trixie as builder
ARG TARGETARCH
ARG TARGETOS

WORKDIR /seaweedfs

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
ARG GOPROXY=https://goproxy.cn,direct
ARG LDFLAGS=""
ARG GOBUILDTAGS=""

COPY go.mod go.sum ./

# Get dependencies - will also be cached if we won't change mod/sum
RUN --mount=type=cache,target=/go/pkg,id=go-mod-cache,sharing=locked \
    go mod download

COPY weed weed
COPY telemetry telemetry

RUN --mount=type=cache,target=/go/pkg,id=go-mod-cache,sharing=locked \
    go build -ldflags "$LDFLAGS" -tags "$GOBUILDTAGS" -o /weed ./weed

FROM vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm-dev/alpine:3.18.4 AS final

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    # apk --no-cache add ca-certificates && \
    apk update && \
    apk add --no-cache fuse && \
    apk add --no-cache curl

RUN mkdir -p /etc/seaweedfs
#COPY ./docker/filer_rocksdb.toml /etc/seaweedfs/filer.toml
COPY ./docker/entrypoint.sh /entrypoint.sh

# Install dependencies and create non-root user
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk upgrade --no-cache && \
    apk add --no-cache fuse curl su-exec && \
    addgroup -g 1000 seaweed && \
    adduser -D -u 1000 -G seaweed seaweed

# volume server grpc port
EXPOSE 18080
# volume server http port
EXPOSE 8080
# filer server grpc port
EXPOSE 18888
# filer server http port
EXPOSE 8888
# master server shared grpc port
EXPOSE 19333
# master server shared http port
EXPOSE 9333
# s3 server http port
EXPOSE 8333
# webdav server http port
EXPOSE 7333

# Create data directory and set proper ownership for seaweed user
RUN mkdir -p /data/filer_rocksdb && \
    chown -R seaweed:seaweed /data && \
    chown -R seaweed:seaweed /etc/seaweedfs && \
    chmod 755 /entrypoint.sh

VOLUME /data
WORKDIR /data

# Entrypoint will handle permission fixes and user switching
ENTRYPOINT ["/entrypoint.sh"]
