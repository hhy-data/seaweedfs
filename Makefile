.PHONY: test admin-generate admin-build admin-clean admin-dev admin-run admin-test admin-fmt admin-help

BINARY = weed
ADMIN_DIR = weed/admin

SOURCE_DIR = .
debug ?= 0

all: install

install: admin-generate
	cd weed; go install

warp_install:
	go install github.com/minio/warp@v0.7.6

full_install: admin-generate
	cd weed; go install -tags "elastic gocdk sqlite ydb tarantool tikv rclone"

server: install
	weed -v 0 server -s3 -filer -filer.maxMB=64 -volume.max=0 -master.volumeSizeLimitMB=100 -volume.preStopSeconds=1 -s3.port=8000 -s3.allowEmptyFolder=false -s3.allowDeleteBucketNotEmpty=true -s3.config=./docker/compose/s3.json -metricsPort=9324

benchmark: install warp_install
	pkill weed || true
	pkill warp || true
	weed server -debug=$(debug) -s3 -filer -volume.max=0 -master.volumeSizeLimitMB=100 -volume.preStopSeconds=1 -s3.port=8000 -s3.allowEmptyFolder=false -s3.allowDeleteBucketNotEmpty=false -s3.config=./docker/compose/s3.json &
	warp client &
	while ! nc -z localhost 8000 ; do sleep 1 ; done
	warp mixed --host=127.0.0.1:8000 --access-key=some_access_key1 --secret-key=some_secret_key1 --autoterm
	pkill warp
	pkill weed

# curl -o profile "http://127.0.0.1:6060/debug/pprof/profile?debug=1"
benchmark_with_pprof: debug = 1
benchmark_with_pprof: benchmark

test: admin-generate
	cd weed; go test -tags "elastic gocdk sqlite ydb tarantool tikv rclone" -v ./...

# Admin component targets
admin-generate:
	@echo "Generating admin component templates..."
	@cd $(ADMIN_DIR) && $(MAKE) generate

admin-build: admin-generate
	@echo "Building admin component..."
	@cd $(ADMIN_DIR) && $(MAKE) build

admin-clean:
	@echo "Cleaning admin component..."
	@cd $(ADMIN_DIR) && $(MAKE) clean

admin-dev:
	@echo "Starting admin development server..."
	@cd $(ADMIN_DIR) && $(MAKE) dev

admin-run:
	@echo "Running admin server..."
	@cd $(ADMIN_DIR) && $(MAKE) run

admin-test:
	@echo "Testing admin component..."
	@cd $(ADMIN_DIR) && $(MAKE) test

admin-fmt:
	@echo "Formatting admin component..."
	@cd $(ADMIN_DIR) && $(MAKE) fmt

admin-help:
	@echo "Admin component help..."
	@cd $(ADMIN_DIR) && $(MAKE) help

test:
	cd weed; go test -tags "elastic gocdk sqlite ydb tikv rclone" -v ./...

GOPROXY ?= https://goproxy.cn,direct
BUILD_LATEST ?= false
IMAGE_PREFIX ?= vdm-registry.cn-hangzhou.cr.aliyuncs.com/udm/
IMAGE_TAG:=$(shell ./docker/image-tag)
TAG ?= ${IMAGE_TAG}
SWCOMMIT=$(shell git rev-parse --short HEAD)
LDFLAGS="-s -w -extldflags '-static' -X 'github.com/seaweedfs/seaweedfs/weed/util.COMMIT=$(SWCOMMIT)'"

filer_migrate_tool: IMAGE := ${IMAGE_PREFIX}filer-migrate:${TAG}
filer_migrate_tool: LATEST_IMAGE := ${IMAGE_PREFIX}filer-migrate:${LATEST_TAG}

IMAGE ?= ${IMAGE_PREFIX}seaweedfs:${TAG}
LATEST_IMAGE ?= ${IMAGE_PREFIX}seaweedfs:${LATEST_TAG}
TAG_FLAGS=-t ${IMAGE} $(if $(findstring $(BUILD_LATEST),true),-t ${LATEST_IMAGE})

filer_mysql:
	docker buildx build --platform linux/amd64,linux/arm64 \
        ${TAG_FLAGS} \
        -f docker/Dockerfile \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

filer_rocksdb:
	docker buildx build --platform linux/amd64,linux/arm64 \
        ${TAG_FLAGS} \
        -f docker/Dockerfile.rocksdb \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

filer_migrate_tool:
	docker buildx build --platform linux/amd64,linux/arm64 \
        ${TAG_FLAGS} \
        -f tools/filer_store_migrate/rocksdb_migrate_tikv/Dockerfile \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .

filer_tikv:
	docker buildx build --platform linux/amd64,linux/arm64 \
        ${TAG_FLAGS} \
        -f docker/Dockerfile \
		--build-arg GOBUILDTAGS="tikv" \
        --build-arg LDFLAGS=${LDFLAGS} \
        --build-arg GOPROXY=${GOPROXY} \
        --push .