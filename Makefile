.PHONY: help setup build test dev stop logs clean

.DEFAULT_GOAL := help
RUN := .run

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

setup: ## Download VM assets and seed env files (idempotent)
	bash scripts/setup.sh

build: ## Build gateway and worker binaries
	cd gateway && go build -o gateway .
	cd worker && go build -o worker .

test: ## Vet + run unit tests in every module
	cd gateway && go vet ./... && go test -count=1 ./...
	cd worker && go vet ./... && go test -count=1 ./...
	cd shared && go vet ./... && go test -count=1 ./...

dev: build ## Start infra + gateway (background) + worker (foreground)
	mkdir -p $(RUN)
	docker compose -f deployment/compose.yml up -d
	(cd gateway && nohup ./gateway > ../$(RUN)/gateway.log 2>&1 & echo $$! > ../$(RUN)/gateway.pid)
	@echo ">> gateway on http://localhost:8080 (logs: .run/gateway.log)"
	@echo ">> starting worker in foreground — ctrl-c to drain and stop"
	cd worker && sudo ./worker

stop: ## Stop gateway, worker and infrastructure containers
	@if [ -f $(RUN)/gateway.pid ]; then kill $$(cat $(RUN)/gateway.pid) 2>/dev/null && rm $(RUN)/gateway.pid; fi
	-sudo pkill -TERM -x worker 2>/dev/null || true
	docker compose -f deployment/compose.yml stop

logs: ## Tail gateway logs
	tail -f $(RUN)/gateway.log

clean: ## Remove built binaries and runtime files
	rm -rf $(RUN) gateway/gateway worker/worker
