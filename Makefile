SHELL := /bin/bash
GO ?= go
BIN := bin
GATEWAY := $(BIN)/gateway
LOADGEN := $(BIN)/loadgen
EVALDET := $(BIN)/evaldet

.PHONY: all build test test-integration lint fmt vet up down bench eval proto clean results

all: build

build:
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(GATEWAY) ./cmd/gateway
	$(GO) build -trimpath -o $(LOADGEN) ./cmd/loadgen
	$(GO) build -trimpath -o $(EVALDET) ./cmd/evaldet

test:
	$(GO) test -count=1 ./...

test-integration:
	$(GO) test -count=1 -tags integration -v ./internal/storage/...

test-race:
	$(GO) test -race -count=1 ./...

lint: fmt vet
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

up:
	docker compose up -d
	@echo "waiting for health..."
	@for i in $$(seq 1 60); do \
		p=$(docker inspect -f '{{.State.Health.Status}}' evg-postgres 2>/dev/null || echo starting); \
		r=$(docker inspect -f '{{.State.Health.Status}}' evg-redis 2>/dev/null || echo starting); \
		if [ "$$p" = "healthy" ] && [ "$$r" = "healthy" ]; then echo "postgres=$$p redis=$$r"; exit 0; fi; \
		sleep 1; \
	done; echo "services did not become healthy in time"; docker compose ps; exit 1

down:
	docker compose down

proto:
	protoc -I proto \
		--go_out=. --go_opt=module=github.com/edgevision/edgevisiongateway \
		--go-grpc_out=. --go-grpc_opt=module=github.com/edgevision/edgevisiongateway \
		proto/edgevision/v1/gateway.proto

bench:
	./scripts/bench.sh

eval:
	$(GO) run ./cmd/evaldet -frames 300 -objects 4 -noise 6 -seed 7

coverage:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=coverage.out | tail -1

clean:
	rm -rf $(BIN) coverage.out
