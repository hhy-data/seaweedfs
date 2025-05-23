BINARY = weed

SOURCE_DIR = .
debug ?= 0

all: install

install:
	cd weed; go install

warp_install:
	go install github.com/minio/warp@v0.7.6

full_install:
	cd weed; go install -tags "elastic gocdk sqlite ydb tikv rclone"

server: install
	weed -v 0 server -s3 -filer -filer.maxMB=64 -volume.max=0 -master.volumeSizeLimitMB=1024 -volume.preStopSeconds=1 -s3.port=8000 -s3.allowEmptyFolder=false -s3.allowDeleteBucketNotEmpty=true -s3.config=./docker/compose/s3.json -metricsPort=9324

benchmark: install warp_install
	pkill weed || true
	pkill warp || true
	weed server -debug=$(debug) -s3 -filer -volume.max=0 -master.volumeSizeLimitMB=1024 -volume.preStopSeconds=1 -s3.port=8000 -s3.allowEmptyFolder=false -s3.allowDeleteBucketNotEmpty=false -s3.config=./docker/compose/s3.json &
	warp client &
	while ! nc -z localhost 8000 ; do sleep 1 ; done
	warp mixed --host=127.0.0.1:8000 --access-key=some_access_key1 --secret-key=some_secret_key1 --autoterm
	pkill warp
	pkill weed

# curl -o profile "http://127.0.0.1:6060/debug/pprof/profile?debug=1"
benchmark_with_pprof: debug = 1
benchmark_with_pprof: benchmark

test:
	cd weed; go test -tags "elastic gocdk sqlite ydb tikv rclone" -v ./...

GOPROXY ?= https://goproxy.cn,direct
IMAGE_PREFIX ?= vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm/
IMAGE_TAG:=$(shell ./docker/image-tag)
TAG ?= ${IMAGE_TAG}
SWCOMMIT=$(shell git rev-parse --short HEAD)
LDFLAGS="-s -w -extldflags '-static' -X 'github.com/seaweedfs/seaweedfs/weed/util.COMMIT=$(SWCOMMIT)'"

filer_migrate_tool: IMAGE := ${IMAGE_PREFIX}filer-migrate:${TAG}
filer_migrate_tool: LATEST_IMAGE := ${IMAGE_PREFIX}filer-migrate:${LATEST_TAG}

IMAGE ?= ${IMAGE_PREFIX}seaweedfs:${TAG}

filer_migrate_tool:
	docker buildx build --platform linux/amd64,linux/arm64 \
        -t ${IMAGE} \
        -f tools/filer_store_migrate/rocksdb_migrate_tikv/Dockerfile \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

# build filer for mysql, tikv, ...
udm:
	docker buildx build --platform linux/amd64,linux/arm64 \
        -t ${IMAGE} \
        -f docker/Dockerfile \
		--build-arg GOBUILDTAGS="tikv" \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

# build filer for rocksdb with cgo=1
udm_rocksdb:
	docker buildx build --platform linux/amd64,linux/arm64 \
        -t ${IMAGE}-rocksdb \
        -f docker/Dockerfile.rocksdb \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

udm_and_rocksdb: udm udm_rocksdb
