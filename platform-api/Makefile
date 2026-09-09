GOLANGCI_LINT ?= golangci-lint

# When not otherwise set, diff/lint against the upstream main branch.
# This is always set in OpenShift CI.
UPSTREAM_REMOTE ?= $(shell git remote -v 2>/dev/null | grep 'openshift-online/gecko.*fetch' | head -1 | cut -f1)
PULL_BASE_SHA ?= $(if $(UPSTREAM_REMOTE),$(shell git rev-parse $(UPSTREAM_REMOTE)/main 2>/dev/null),$(shell git rev-parse main 2>/dev/null))

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run --config ./.golangci.yml --modules-download-mode=readonly -v \
		$(if $(PULL_BASE_SHA),--new-from-rev=$(PULL_BASE_SHA))

.PHONY: lint-fix
lint-fix:
	$(GOLANGCI_LINT) run --config ./.golangci.yml --fix -v \
		$(if $(PULL_BASE_SHA),--new-from-rev=$(PULL_BASE_SHA) --whole-files)

.PHONY: lint-fmt
lint-fmt:
	$(GOLANGCI_LINT) fmt --config ./.golangci.yml

.PHONY: generate build test server-start server-stop server-logs

generate:
	@echo "Generating public APIs, schemas, and deepcopy methods..."
	@go run ./cmd/orlop-gen
	@echo "Done!"

build:
	go build ./...

test:
	go test ./...

# Local server for testing (memory backend)
server-start:
	@echo "Starting platform-api-server (memory backend)..."
	@go build -o /tmp/platform-api-server ./cmd/platform-api-server
	@env -u SPANNER_DATABASE -u DB_HOST nohup /tmp/platform-api-server --disable-auth --address=127.0.0.1 --private-port=18080 --public-port=18081 > /tmp/platform-api-server.log 2>&1 &
	@sleep 2
	@if pgrep -f "platform-api-server --disable-auth" > /dev/null; then \
		echo "Server started (PID $$(pgrep -f 'platform-api-server --disable-auth'))"; \
		echo "Private API: https://127.0.0.1:18080 (skip TLS verification)"; \
		echo "Public API:  http://127.0.0.1:18081"; \
	else \
		echo "Server failed to start, check /tmp/platform-api-server.log"; \
		exit 1; \
	fi

server-stop:
	@echo "Stopping platform-api-server..."
	@pkill -f "platform-api-server --disable-auth" || echo "Server not running"
	@sleep 1

server-logs:
	@tail -f /tmp/platform-api-server.log
