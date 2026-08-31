# UniRate 开发与运维入口
#
# 本机无需安装 Go —— 所有构建与测试均在容器内执行，
# 保证本地产物与 CI / 生产环境完全一致。

SHELL := /bin/bash
# 必须 >= go.mod 的 go 指令版本。modernc.org/sqlite 把下限抬到了 1.25，
# 用更低的镜像会在 go mod 校验阶段直接失败（报 "requires go >= 1.25"）。
GO_IMAGE := golang:1.25-alpine
GOPROXY ?= https://goproxy.cn,direct
# 默认关 CGO：SQLite 用的是 modernc.org/sqlite 纯 Go 实现，
# 生产镜像也以 CGO_ENABLED=0 静态链接。只有 -race 需要 cgo，
# 因此仅 test 目标单独覆盖为 1（见下方 test:）。
CGO_ENABLED ?= 0
TEST_REDIS := unirate-test-redis
TEST_REDIS_PORT ?= 16399
PWD_ABS := $(shell pwd)

DOCKER_GO = docker run --rm \
	-v "$(PWD_ABS)":/app \
	-v unirate-gomod:/go/pkg/mod \
	-w /app \
	-e GOPROXY=$(GOPROXY) \
	-e CGO_ENABLED=$(CGO_ENABLED) \
	-e REDIS_ADDR=host.docker.internal:$(TEST_REDIS_PORT) \
	--add-host=host.docker.internal:host-gateway \
	$(GO_IMAGE)

.DEFAULT_GOAL := help
.PHONY: help init tidy fmt vet lint test test-short redis-up redis-down \
        build up up-test down logs e2e ps clean verify

help: ## 显示可用目标
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

init: ## 生成含强随机凭证的 .env（已存在则拒绝覆盖）
	@bash scripts/init-env.sh

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

# -race 依赖 cgo，故本目标用 target-specific 变量把 CGO 覆盖为 1 并装 gcc。
# 注意必须用 make 的 target-specific 赋值：DOCKER_GO 里的 $(CGO_ENABLED)
# 在配方展开时求值，命令行前缀 CGO_ENABLED=1 只影响 shell，改不到它。
test: CGO_ENABLED := 1
test: redis-up ## 全量测试（含 race 检测）
	$(DOCKER_GO) sh -c "apk add --no-cache gcc musl-dev >/dev/null 2>&1 && \
		go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./... && \
		go tool cover -func=coverage.out | tail -20"

test-short: redis-up ## 快速测试（跳过 race）
	$(DOCKER_GO) go test -count=1 ./...

# 生产编排只含 redis/gateway（SQLite 随网关进程内嵌，无独立数据库容器）；
# 测试夹具（mock-upstream + demo 种子数据）由 overlay 叠加。
# TEST_COMPOSE 供 e2e/perf 等测试目标使用。
TEST_COMPOSE := docker compose -f docker-compose.yml -f docker-compose.test.yml

build: ## 构建镜像
	docker compose build

up: ## 启动完整栈（生产编排，不含测试夹具）
	docker compose up -d --build
	@echo "等待网关就绪..."
	@for i in $$(seq 1 60); do \
		curl -fsS http://127.0.0.1:$${OBS_PORT:-29091}/ready >/dev/null 2>&1 && \
			{ echo "就绪（耗时 $${i}s）"; exit 0; }; \
		sleep 1; done; \
		echo "启动超时"; docker compose logs --tail=100; exit 1

up-test: ## 启动完整栈 + 测试夹具（mock-upstream/demo 数据），e2e 与压测用
	$(TEST_COMPOSE) up -d --build
	@echo "等待网关就绪..."
	@for i in $$(seq 1 60); do \
		curl -fsS http://127.0.0.1:$${OBS_PORT:-29091}/ready >/dev/null 2>&1 && \
			{ echo "就绪（耗时 $${i}s）"; exit 0; }; \
		sleep 1; done; \
		echo "启动超时"; $(TEST_COMPOSE) logs --tail=100; exit 1

# 拆除与观测统一带 overlay：overlay 的服务集是生产编排的超集，
# 不带它则 down 看不见 mock-upstream，残留端点会让网络删除失败
# （报 "has active endpoints"），下一次 e2e 直接起不来。
down: ## 停止并清理数据卷（含测试夹具）
	$(TEST_COMPOSE) down -v

logs: ## 跟踪网关日志
	$(TEST_COMPOSE) logs -f gateway

ps: ## 查看服务状态（含测试夹具）
	$(TEST_COMPOSE) ps

# 种子 SQL 由 store.LoadSeeds 在每次启动时幂等重放（仅 SEED_SQL_DIR 生效），
# 所以 demo 业务域本身不依赖清卷。这里仍 down -v 是为了保证验收基线干净：
# 上一轮用例通过 Admin API 改过的规则、Token 预算余量都会留在
# sqlite-data 卷与 redis 里，不清会让断言受历史状态影响而偶发失败。
e2e: ## 端到端验收（自动重建含测试夹具的干净栈）
	$(TEST_COMPOSE) down -v
	$(MAKE) up-test
	./test/e2e/run.sh

verify: vet test ## 提交前完整校验
	@echo "校验通过"

clean: redis-down ## 清理全部本地资源
	-$(TEST_COMPOSE) down -v
	-docker volume rm unirate-gomod
	-rm -f coverage.out
