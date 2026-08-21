# dat_recover: recovers filer metadata from volume .dat files that stored
# the topics/.system/log needles. Output is fs.meta.load compatible.
#
# Build (from repo root, build context must be the repo root):
#   docker build -f unmaintained/dat_recover/Dockerfile -t dat-recover:dev .
#
# Run: the container stays idle (tail -f); exec into it to run the tool:
#   docker run -d --name dat-recover -v /mnt/weed1:/data dat-recover:dev
#   docker exec -it dat-recover sh
#   # dat_recover -datList=/data/list.txt -output=/data/recovered.meta

FROM --platform=${BUILDPLATFORM} vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm-dev/golang:1.26.2-trixie AS builder
ARG TARGETARCH
ARG TARGETOS

WORKDIR /seaweedfs

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
ARG GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg,id=go-mod-cache,sharing=locked \
    go mod download

COPY weed weed
COPY unmaintained/dat_recover unmaintained/dat_recover

RUN go build -o /out/dat_recover ./unmaintained/dat_recover/

FROM vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm-dev/alpine:3.18.4

# aliyun apk mirror, same as docker/Dockerfile.ok
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories

COPY --from=builder /out/dat_recover /usr/local/bin/dat_recover

# usage guide, view inside the container with: cat /README.md
COPY unmaintained/dat_recover/README.md /README.md

# expected mount point for volume .dat files
WORKDIR /data

ENTRYPOINT ["tail", "-f", "/dev/null"]
