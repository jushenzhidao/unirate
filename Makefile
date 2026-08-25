# UniRate 开发与运维入口
#
# 本机无需安装 Go —— 所有构建与测试均在容器内执行，
# 保证本地产物与 CI / 生产环境完全一致。

SHELL := /bin/bash
GO_IMAGE := golang:1.22-alpine
GOPROXY ?= https://goproxy.cn,direct
TEST_REDIS := unirate-test-redis
TEST_REDIS_PORT ?= 16399
PWD_ABS := $(shell pwd)

DOCKER_GO = docker run --rm \
	-v "$(PWD_ABS)":/app \
	-v unirate-gomod:/go/pkg/mod \
	-w /app \
	-e GOPROXY=$(GOPROXY) \
	-e CGO_ENABLED=1 \
	-e REDIS_ADDR=host.docker.internal:$(TEST_REDIS_PORT) \
	--add-host=host.docker.internal:host-gateway \
	$(GO_IMAGE)

.DEFAULT_GOAL := help
.PHONY: help init tidy fmt vet lint test test-short redis-up redis-down \
        build up down logs e2e ps clean verify migrate

help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

init: ## 生成含强随机凭证的 .env（已存在则拒绝覆盖）
	@bash scripts/init-env.sh

migrate: ## 对已运行的 MySQL 应用增量迁移（新部署由 init.sql 自动完成）
	@set -euo pipefail; \
	[ -f .env ] || { echo "缺少 .env，请先执行 make init"; exit 1; }; \
	root_pw=$$(grep -E '^MYSQL_ROOT_PASSWORD=' .env | cut -d= -f2-); \
	[ -n "$$root_pw" ] || { echo ".env 中未找到 MYSQL_ROOT_PASSWORD"; exit 1; }; \
	for f in deploy/mysql/migrations/*.sql; do \
		echo "应用 $$f"; \
		docker compose exec -T mysql \
			mysql -u root -p"$$root_pw" unirate < "$$f"; \
	done; \
	echo "迁移完成"

tidy: ## 同步 go.mod / go.sum
	$(DOCKER_GO) go mod tidy

fmt: ## 格式化代码
	$(DOCKER_GO) gofmt -w -l .

vet: ## 静态检查
	$(DOCKER_GO) go vet ./...

lint: ## golangci-lint
	docker run --rm -v "$(PWD_ABS)":/app -w /app \
		golangci/golangci-lint:v1.59-alpine golangci-lint run --timeout=5m

redis-up: ## 启动测试用 Redis
	@docker inspect $(TEST_REDIS) >/dev/null 2>&1 || \
		docker run -d --name $(TEST_REDIS) -p $(TEST_REDIS_PORT):6379 redis:7.2-alpine >/dev/null
	@for i in $$(seq 1 20); do \
		docker exec $(TEST_REDIS) redis-cli ping >/dev/null 2>&1 && break; sleep 0.5; done
	@echo "测试 Redis 就绪 (127.0.0.1:$(TEST_REDIS_PORT))"

redis-down: ## 移除测试用 Redis
	-docker rm -f $(TEST_REDIS) >/dev/null 2>&1

test: redis-up ## 全量测试（含 race 检测）
	$(DOCKER_GO) sh -c "apk add --no-cache gcc musl-dev >/dev/null 2>&1 && \
		go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./... && \
		go tool cover -func=coverage.out | tail -20"

test-short: redis-up ## 快速测试（跳过 race）
	$(DOCKER_GO) go test -count=1 ./...

build: ## 构建镜像
	docker compose build

up: ## 启动完整栈
	docker compose up -d --build
	@echo "等待网关就绪..."
	@for i in $$(seq 1 60); do \
		curl -fsS http://127.0.0.1:$${OBS_PORT:-29091}/ready >/dev/null 2>&1 && \
			{ echo "就绪（耗时 $${i}s）"; exit 0; }; \
		sleep 1; done; \
		echo "启动超时"; docker compose logs --tail=100; exit 1

down: ## 停止并清理数据卷
	docker compose down -v

logs: ## 跟踪网关日志
	docker compose logs -f gateway

ps: ## 查看服务状态
	docker compose ps

e2e: ## 端到端验收
	./test/e2e/run.sh

verify: vet test ## 提交前完整校验
	@echo "校验通过"

clean: redis-down ## 清理全部本地资源
	-docker compose down -v
	-docker volume rm unirate-gomod
	-rm -f coverage.out
