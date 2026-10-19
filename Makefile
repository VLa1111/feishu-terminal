APP := feishu-terminal

# 版本信息：没有 git 仓库时从 VERSION 文件读，构建时注入二进制。
VERSION ?= $(shell cat VERSION 2>/dev/null || echo 0.0.0)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

.PHONY: build run test image up down logs restart pack dist serve-dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(APP) .

# 本地直跑（不注入版本号以外的任何东西）
run: build
	./$(APP)

test:
	go test ./...

image:
	docker compose build

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f

restart:
	docker compose restart

# 直接执行 ./feishu-terminal 时打印版本（可用来验证构建注入是否生效）
version: build
	./$(APP) --version

# 交叉编译发布包：dist/*.tar.gz（新服务器部署用）+ 裸二进制 + release.json（/upgrade 用）
dist:
	bash scripts/release.sh "$(VERSION)"

# 本机起一个静态 HTTP 服务，方便局域网里的 bot 直接 /upgrade
serve-dist:
	cd dist && python3 -m http.server 18080

# 打一个干净源码包（排除二进制/.env/本地缓存），方便 scp 到服务器
pack:
	tar czf $(APP)-src.tar.gz \
		--exclude='.git' \
		--exclude='.env' \
		--exclude='$(APP)' \
		--exclude='$(APP).rollback*' \
		--exclude='$(APP)-src.tar.gz' \
		--exclude='dist' \
		--exclude='zsh/.zcompdump*' \
		.

clean:
	rm -f $(APP) $(APP)-src.tar.gz $(APP).rollback $(APP).rollback.tmp
	rm -rf dist
