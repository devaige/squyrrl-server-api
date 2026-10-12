SHELL := /bin/bash

BIN_DIR := bin
APP := squyrrl-api

.PHONY: help build tidy fmt vet test migrate-create

help:
	@echo "可用目标："
	@echo "  make build            编译 API 二进制至 $(BIN_DIR)/$(APP)"
	@echo "  make tidy             go mod tidy"
	@echo "  make fmt              gofmt -s -w ."
	@echo "  make vet              go vet ./..."
	@echo "  make test             go test ./..."
	@echo "  make migrate-create name=xxx   生成新迁移文件"

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
