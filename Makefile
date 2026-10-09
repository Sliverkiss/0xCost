# mimo2api delivery Makefile (docs/TDD.md §3.11 Docker/delivery, §6 acceptance).
# Go tools are not on the default PATH on this host, so prepend the toolchain.
export PATH := /usr/local/go/bin:$(PATH)

GO         ?= go
IMAGE      ?= mimo2api:test
COVER_DIR  := .cover
# internal packages that carry coverage floors (docs/TDD.md §6.2).
PKGS       := adapter pool mimo api accounts config

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
	check adapter 90; check pool 85; check mimo 80; \
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
# git-ignored data/ (and references/). The search terms come from the credential
# files themselves — never hardcoded here, since this file is part of the
# repository — and the extraction lives in scripts/sensitive-values.py, which
# covers BOTH sources: data/accounts.json (account identifiers) and
# data/deploy.env (store credentials, API keys, egress credentials). A store
# credential opens the pool exactly like a password does, so the gate must see it
# (ADR 0020 — the inline copy this replaced only ever read the account file, and
# an Upstash endpoint reached a tracked ADR through that hole).
hygiene: ## Fail if real credentials (account identifiers, store/deploy credentials) leak into the repo.
	@fail=0; \
	if command -v python3 >/dev/null 2>&1; then \
		real=$$(python3 scripts/sensitive-values.py data/accounts.json data/deploy.env; echo "$$MIMO2API_HYGIENE_EXTRA"); \
		if [ -z "$$(echo $$real | tr -d '[:space:]')" ]; then \
			echo "hygiene: no credential files present (or they are empty) — nothing to compare"; \
		fi; \
		for t in $$real; do \
			[ -n "$$t" ] || continue; \
			if grep -rIn --exclude-dir=.git --exclude-dir=data --exclude-dir=references \
				--exclude=Makefile --exclude=docker_test.sh -F -- "$$t" . ; then \
				echo "hygiene: FAIL — a real credential is present in the repo"; fail=1; \
			fi; \
		done; \
	else \
		echo "hygiene: no python3 — only the static scans ran"; \
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

# The published mirror (see .gitattributes): the tree without the test suite, as a
# single fresh commit, pushed to MIRROR_URL. It never sends the local history — older
# commits predate the redaction of real account identifiers.
MIRROR_URL ?= https://github.com/Sliverkiss/mimo2api
MIRROR_DIR ?= $(CURDIR)/../.mimo2api-mirror
MIRROR_MSG ?= docs/mirror-commit-message.txt

mirror: ## Build the distribution mirror at $(MIRROR_DIR).
	rm -rf $(MIRROR_DIR) && mkdir -p $(MIRROR_DIR)
	git archive HEAD | tar -x -C $(MIRROR_DIR)
	@# excluding a directory's contents can leave its empty shell behind
	@find $(MIRROR_DIR) -type d -empty -delete
	@echo "mirror built: $$(find $(MIRROR_DIR) -type f | wc -l) files"

push-mirror: mirror ## Build it and push it to $(MIRROR_URL) as a single commit.
	cd $(MIRROR_DIR) && git init -q && git add -A \
		&& git -c user.name="$$(git -C $(CURDIR) config user.name)" \
			-c user.email="$$(git -C $(CURDIR) config user.email)" \
			commit -q -F "$$(git -C $(CURDIR) rev-parse --show-toplevel)/$(MIRROR_MSG)" \
		&& git branch -M main \
		&& git push --force $(MIRROR_URL) main
