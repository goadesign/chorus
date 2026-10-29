#! /usr/bin/make
#
# Makefile for Chorus
#
# Targets:
# - "depend" updates Go module dependencies and installs code/diagram generators
# - "gen" regenerates Goa and Goa-AI MCP server/client code from design/design.go
# - "diagram" renders diagram/Chorus Architecture.svg and diagram/design.json via mdl
# - "lint" runs go vet across all packages
# - "test" runs the test suite with the race detector
# - "release" regenerates code and diagrams, runs preflight checks, tags and pushes
#   the release, builds multi-arch release archives, and publishes to GitHub.
#
# Meta targets:
# - "all" is the default target; it runs "gen", "diagram", "lint", and "test"
#
MAJOR=0
MINOR=2
BUILD=0

VERSION=$(MAJOR).$(MINOR).$(BUILD)
RELEASE_TAG?=v$(VERSION)

GOPATH=$(shell go env GOPATH)
GOBIN_DIR=$(GOPATH)/bin
DIST_DIR=dist

.PHONY: all depend gen diagram lint test test-release release-preflight build-release-assets warm-proxy release
.NOTPARALLEL: release release-preflight build-release-assets warm-proxy

all: gen diagram lint test

depend:
	@echo "Updating Go module dependencies..."
	@mkdir -p "$(GOBIN_DIR)"
	@go get -u -v ./...
	@go mod tidy
	@GOBIN="$(GOBIN_DIR)" go install goa.design/goa/v3/cmd/goa@latest
	@GOBIN="$(GOBIN_DIR)" go install goa.design/model/cmd/mdl@latest

gen:
	@PATH="$(GOBIN_DIR):$$PATH" goa gen github.com/goadesign/chorus/design

diagram:
	@PATH="$(GOBIN_DIR):$$PATH" mdl svg github.com/goadesign/chorus/diagram -all -dir diagram -out diagram/design.json
	@PATH="$(GOBIN_DIR):$$PATH" mdl gen github.com/goadesign/chorus/diagram -out diagram/design.json

lint:
	@go vet ./...

test:
	@go test -race ./...

test-release:
	@go test -count=1 -race ./...

release-preflight: gen diagram lint test-release

build-release-assets:
	@rm -rf "$(DIST_DIR)"
	@mkdir -p "$(DIST_DIR)"
	@for target in "darwin amd64" "darwin arm64" "linux amd64" "linux arm64"; do \
		set -- $$target; \
		os="$$1"; \
		arch="$$2"; \
		stage="$(DIST_DIR)/stage_$${os}_$${arch}"; \
		mkdir -p "$$stage"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -trimpath -ldflags="-s -w" -o "$$stage/chorus" ./cmd/chorus; \
		cp README.md LICENSE "$$stage/"; \
		tar -C "$$stage" -czf "$(DIST_DIR)/chorus_$(VERSION)_$${os}_$${arch}.tar.gz" chorus README.md LICENSE; \
		rm -rf "$$stage"; \
	done
	@cd "$(DIST_DIR)" && shasum -a 256 *.tar.gz > checksums.txt

warm-proxy:
	@tag="$(RELEASE_TAG)"; \
	commit="$$(git rev-parse "refs/tags/$$tag^{commit}")"; \
	echo "Warming proxy.golang.org and sum.golang.org for github.com/goadesign/chorus@$$tag ($$commit)..."; \
	for i in $$(seq 1 30); do \
		if curl -fsSL "https://proxy.golang.org/github.com/goadesign/chorus/@v/$$commit.info" >/dev/null 2>&1 && \
		   curl -fsSL "https://sum.golang.org/lookup/github.com/goadesign/chorus@$$tag" >/dev/null 2>&1; then \
			break; \
		fi; \
		if [ "$$i" -eq 30 ]; then \
			echo "warning: proxy.golang.org or sum.golang.org did not index $$tag in time"; \
			break; \
		fi; \
		sleep 2; \
	done; \
	curl -fsSL "https://proxy.golang.org/github.com/goadesign/chorus/@v/$$tag.info" >/dev/null 2>&1 || true

release:
	@test "$$(git branch --show-current)" = "main" || (echo "error: release must run from main branch" && exit 1)
	@status="$$(git status --porcelain)"; \
	if [ -n "$$status" ]; then \
		echo "error: chorus repo has uncommitted changes:"; \
		echo "$$status"; \
		exit 1; \
	fi
	@if git rev-parse --verify --quiet "refs/tags/$(RELEASE_TAG)" >/dev/null; then \
		echo "error: local tag $(RELEASE_TAG) already exists"; \
		exit 1; \
	fi
	@if git ls-remote --exit-code --tags origin "refs/tags/$(RELEASE_TAG)" >/dev/null 2>&1; then \
		echo "error: remote tag $(RELEASE_TAG) already exists"; \
		exit 1; \
	fi
	sed 's/Version(".*")/Version("$(VERSION)")/' design/design.go > _tmp && mv _tmp design/design.go
	sed 's/Version(".*")/Version("$(VERSION)")/' diagram/model/model.go > _tmp && mv _tmp diagram/model/model.go
	$(MAKE) release-preflight
	@if [ -n "$$(git status --porcelain)" ]; then \
		git add .; \
		git commit -m "Release $(RELEASE_TAG)"; \
	fi
	git tag "$(RELEASE_TAG)"
	git push origin main
	git push origin "$(RELEASE_TAG)"
	$(MAKE) build-release-assets
	gh release create "$(RELEASE_TAG)" "$(DIST_DIR)"/*.tar.gz "$(DIST_DIR)/checksums.txt" \
		--repo goadesign/chorus \
		--title "Chorus $(RELEASE_TAG)" \
		--generate-notes
	@rm -rf "$(DIST_DIR)"
	@$(MAKE) warm-proxy RELEASE_TAG="$(RELEASE_TAG)"
	@echo "Release $(RELEASE_TAG) complete"
