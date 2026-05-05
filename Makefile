.PHONY: all vendor build clean run watch image

NPM ?= npm
VERSION ?= $(shell date -u +%Y%m%d-%H%M%S)
CONTAINER_ENGINE ?= $(shell command -v podman 2>/dev/null || command -v docker)
IMAGE ?= tmpshare
IMAGE_TAG ?= $(VERSION)

all: build

# Bundle JS — installe les deps si node_modules manquant.
vendor: web/vendor/app.js

web/vendor/app.js: web/src/app.js package.json
	@[ -d node_modules ] || $(NPM) install --silent --no-audit --no-fund
	@mkdir -p web/vendor
	$(NPM) run build

# Watch pendant le dev (rebuild automatique).
watch:
	@[ -d node_modules ] || $(NPM) install --silent --no-audit --no-fund
	@mkdir -p web/vendor
	npx esbuild web/src/app.js --bundle --format=esm --target=es2020 --sourcemap --outfile=web/vendor/app.js --watch

# Binaire Go (embed inclut web/vendor/app.js).
build: vendor
	go build -ldflags "-X main.buildVersion=$(VERSION)" -o tmpshare .

run: build
	./tmpshare

# Image OCI (podman ou docker selon ce qui est disponible).
image:
	$(CONTAINER_ENGINE) build -f Containerfile -t $(IMAGE):$(IMAGE_TAG) -t $(IMAGE):latest .

clean:
	rm -rf node_modules web/vendor tmpshare
