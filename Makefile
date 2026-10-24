APP     := cilicili
PKG     := ./cmd/cilici
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build run test race vet fmt tidy linux clean

help:
	@echo "make build   # 本机构建 bin/$(APP)"
	@echo "make run     # 本地运行（需要 .env 或环境变量）"
	@echo "make test    # go test ./..."
	@echo "make race    # go test -race ./..."
	@echo "make linux   # 交叉编译 linux/amd64 到 bin/$(APP)-linux-amd64"
	@echo "make vet/fmt/tidy"
	@echo "make web-deps/web-build/web-test/web-test-browser/web-serve # Flutter 3.35.7"

build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP) $(PKG)

run:
	go run ./cmd/cilici

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './web/*')

tidy:
	go mod tidy

linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(APP)-linux-amd64 $(PKG)

clean:
	rm -rf bin

# Fladder v0.11.1 uses Flutter 3.35.7 (clients/fladder/.fvmrc).
FLUTTER ?= flutter
.PHONY: web-deps web-build web-test web-serve
web-deps:
	cd clients/fladder && $(FLUTTER) pub get

web-build:
	cd clients/fladder && $(FLUTTER) build web --no-wasm-dry-run --no-web-resources-cdn --pwa-strategy=none

web-test:
	cd clients/fladder && $(FLUTTER) test test/cilicili_client_test.dart test/films_test.dart test/catalog_cache_test.dart test/font_theme_test.dart test/cilicili_settings_test.dart test/cilicili_catalog_test.dart test/cilicili_logout_test.dart
	python3 scripts/test_serve_web.py

web-serve:
	python3 scripts/serve_web.py

# Requires Chrome; exercises the Web-only HLS bridge without external video sources.
.PHONY: web-test-browser
web-test-browser:
	cd clients/fladder && $(FLUTTER) test --platform chrome test/web_hls_test.dart test/catalog_cache_test.dart test/cilicili_settings_test.dart test/web_route_test.dart test/cilicili_logout_test.dart test/cilicili_catalog_test.dart
