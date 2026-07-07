SHELL := /bin/bash

# dev 依赖栈已统一归置到 server/（与生产 compose 并排）
DOCKER_COMPOSE := docker compose -f ../../docker-compose.dev.yml
BIN_DIR := bin
APP := squyrrl-api

.PHONY: help dev infra infra-down infra-clean run build tidy fmt vet test migrate-create

help:
	@echo "可用目标："
	@echo "  make dev              起开发依赖并运行 API"
	@echo "  make infra            起开发依赖容器（postgres/minio）"
	@echo "  make infra-down       停止依赖容器"
	@echo "  make infra-clean      停止并清空依赖卷"
	@echo "  make run              直接运行 API（依赖需先就绪）"
	@echo "  make build            编译 API 二进制至 $(BIN_DIR)/$(APP)"
	@echo "  make tidy             go mod tidy"
	@echo "  make fmt              gofmt -s -w ."
	@echo "  make vet              go vet ./..."
	@echo "  make test             go test ./..."
	@echo "  make migrate-create name=xxx   生成新迁移文件"

dev: infra run

# 只起依赖容器（应用已容器化，全套由 server 的 dev compose 起；此目标供原生逃生跑 API 时用）
infra:
	$(DOCKER_COMPOSE) up -d postgres minio
	@echo "等待依赖就绪..."
	@sleep 3

infra-down:
	$(DOCKER_COMPOSE) down

infra-clean:
	$(DOCKER_COMPOSE) down -v

run:
	@if [ ! -f .env ]; then \
		echo "未找到 .env，使用 .env.example 默认值"; \
		cp .env.example .env; \
	fi
	go run ./cmd/api

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(APP) ./cmd/api

tidy:
	go mod tidy

fmt:
	gofmt -s -w .

vet:
	go vet ./...

test:
	go test ./...

# 用法：make migrate-create name=add_users
migrate-create:
	@if [ -z "$(name)" ]; then \
		echo "用法：make migrate-create name=<迁移名称>"; \
		exit 1; \
	fi
	@ts=$$(date +%Y%m%d%H%M%S); \
	dir=internal/infra/db/migrations; \
	mkdir -p $$dir; \
	f=$$dir/$${ts}_$(name).sql; \
	printf -- "-- +goose Up\n\n-- +goose Down\n" > $$f; \
	echo "已创建 $$f"
