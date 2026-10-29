APP     := cilicili
PKG     := ./cmd/cilici
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# -buildvcs=false：不把 git 提交信息编进二进制，否则每次提交哈希都会变，无法用哈希校验「部署的是不是同一份代码」
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS_REPRO := -trimpath -buildvcs=false

.PHONY: help build run test race vet fmt tidy linux clean docker docker-up

help:
	@echo "make build   # 本机构建 bin/$(APP)"
	@echo "make run     # 本地运行（需要 .env 或环境变量）"
	@echo "make test    # go test ./..."
	@echo "make race    # go test -race ./..."
	@echo "make linux   # 交叉编译 linux/amd64 到 bin/$(APP)-linux-amd64"
	@echo "make vet/fmt/tidy"
	@echo "make docker/docker-up  # 构建/启动后端 Docker 镜像"

build:
	@mkdir -p bin
	go build $(GOFLAGS_REPRO) -ldflags "$(LDFLAGS)" -o bin/$(APP) $(PKG)

run:
	go run ./cmd/cilici

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go')

tidy:
	go mod tidy

linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS_REPRO) -ldflags "$(LDFLAGS)" -o bin/$(APP)-linux-amd64 $(PKG)

clean:
	rm -rf bin

.PHONY: docker docker-up
docker:
	docker compose build

docker-up:
	docker compose up -d
