# mimo2api delivery Makefile (docs/TDD.md §3.11 Docker/delivery, §6 acceptance).
# Go tools are not on the default PATH on this host, so prepend the toolchain.
export PATH := /usr/local/go/bin:$(PATH)

GO         ?= go
IMAGE      ?= mimo2api:test
COVER_DIR  := .cover
# internal packages that carry coverage floors (docs/TDD.md §6.2).
PKGS       := adapter pool session mimo api accounts config

.DEFAULT_GOAL := help
.PHONY: help build test test-race cover cover-check vet fmt hygiene test-docker run clean

help: ## List available targets.
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile every package (go build ./...).
	$(GO) build ./...

test: ## Run the mock suite (go test ./...; LIVE-*/DOCKER-* skips are expected).
	$(GO) test ./...

test-race: ## Run the whole suite under the race detector (required gate, §1.5).
	$(GO) test -race ./...

cover: ## Coverage: merged cover.out + per-internal-package percentage.
	@mkdir -p $(COVER_DIR)
	@$(GO) test -coverprofile=cover.out ./... >/dev/null 2>&1 || true
	@for p in $(PKGS); do \
		$(GO) test -coverprofile=$(COVER_DIR)/$$p.out ./internal/$$p >/dev/null 2>&1 || true; \
		pct=$$($(GO) tool cover -func=$(COVER_DIR)/$$p.out 2>/dev/null | awk '/^total:/{print $$3}'); \
		printf '  internal/%-9s %s\n' "$$p" "$$pct"; \
	done
	@echo "  cover.out written (merged ./...)"

cover-check: cover ## Fail with a clear message if a package is below its §6.2 floor.
	@fail=0; \
	check() { \
		p=$$1; floor=$$2; \
		got=$$($(GO) tool cover -func=$(COVER_DIR)/$$p.out 2>/dev/null | awk '/^total:/{gsub(/%/,"",$$3); print $$3}'); \
		if awk -v g="$$got" -v f="$$floor" 'BEGIN{exit !(g<f)}'; then \
			echo "cover-check: FAIL internal/$$p $${got}% < floor $${floor}%"; fail=1; \
		else \
			echo "cover-check: ok   internal/$$p $${got}% (floor $${floor}%)"; \
		fi; }; \
	check adapter 90; check pool 85; check session 90; check mimo 80; \
	check api 70; check accounts 80; check config 80; \
	[ $$fail -eq 0 ] || { echo "cover-check: FAILED — package(s) below floor (docs/TDD.md §6.2)"; exit 1; }; \
	echo "cover-check: all packages meet their floors"

vet: ## Run go vet ./...
	$(GO) vet ./...

fmt: ## Fail if gofmt -l internal cmd is non-empty.
	@out=$$(gofmt -l internal cmd); \
	if [ -n "$$out" ]; then \
		echo "fmt: FAIL — these files need gofmt:"; echo "$$out"; exit 1; \
	fi; \
	echo "fmt: clean"

# Repo hygiene (docs/TDD.md §6.7): no real credentials anywhere except the
# git-ignored data/ (and references/). The two designated phone numbers are
# search terms here, so this Makefile excludes itself from the scan.
hygiene: ## Fail if real credentials (phones/password/pass+serviceToken) leak into the repo.
	@fail=0; \
	for t in 18278724616 19301307763; do \
		if grep -rIn --exclude-dir=.git --exclude-dir=data --exclude-dir=references \
			--exclude=Makefile --exclude=docker_test.sh -F -- "$$t" . ; then \
			echo "hygiene: FAIL — real phone number present in repo"; fail=1; \
		fi; \
	done; \
	if [ -f data/accounts.json ] && command -v python3 >/dev/null 2>&1; then \
		real=$$(python3 -c 'import json,re;d=json.load(open("data/accounts.json"));[print(v) for a in d.get("accounts",[]) for v in [a.get("password"),a.get("pass_token")] if v];[print(m.group(1)) for a in d.get("accounts",[]) for m in [re.search(r"serviceToken=\"?([^\";]+)",(a.get("session") or {}).get("cookie","") or "")] if m]' 2>/dev/null); \
		for t in $$real; do \
			[ -n "$$t" ] || continue; \
			if grep -rIn --exclude-dir=.git --exclude-dir=data --exclude-dir=references \
				--exclude=Makefile --exclude=docker_test.sh -F -- "$$t" . ; then \
				echo "hygiene: FAIL — real credential value present in repo"; fail=1; \
			fi; \
		done; \
	fi; \
	[ $$fail -eq 0 ] || { echo "hygiene: FAILED (docs/TDD.md §6.7)"; exit 1; }; \
	echo "hygiene: clean — no real credentials outside data/"

# The tool-calling middleware (ADR 0012) is vendored: run its own pytest suite.
# Containerised so the Go toolchain needs no Python environment. Its `live`-marked
# test needs real upstream credentials and is excluded.
test-toolforge:
	docker run --rm -v "$$PWD/deploy/toolforge:/app" -w /app python:3.12-slim \
		sh -c "pip install -q -r requirements.txt pytest pytest-asyncio && python -m pytest -q -m 'not live'"

test-docker: ## Run the DOCKER-001..006 harness (skips when docker is absent).
	@bash docker/docker_test.sh

run: ## Run the gateway locally with the ambient env config.
	$(GO) run ./cmd/mimo2api

clean: ## Remove build and coverage artifacts.
	@rm -rf $(COVER_DIR) cover.out mimo2api
	@$(GO) clean ./... >/dev/null 2>&1 || true
