VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test lint run demo docker release-dry version-check clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o pgcellar ./cmd/pgcellar

test:
	go test -race ./...

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "golangci-lint not found, skipped"; fi

run: build
	PATH="$(CURDIR)/internal/barman/testdata:$$PATH" ./pgcellar serve --config deploy/pgcellar.example.yaml --dev

# The public demo: fictional data, read-only, user demo / demo, on http://127.0.0.1:8080
demo: build
	./pgcellar serve --demo --listen 127.0.0.1:8080

docker:
	docker build -f deploy/Dockerfile -t pgcellar:dev --build-arg VERSION=$(VERSION) .

# Build the release archives locally (linux amd64/arm64, darwin arm64) and their
# checksums into dist/, publish nothing. Falls back to plain cross builds when
# goreleaser is not installed.
release-dry:
	@if command -v goreleaser >/dev/null 2>&1; then \
		goreleaser release --snapshot --clean --skip=publish; \
	else \
		echo "goreleaser not found, cross-compiling the three targets instead"; \
		rm -rf dist && mkdir -p dist; \
		for t in linux/amd64 linux/arm64 darwin/arm64; do \
			CGO_ENABLED=0 GOOS=$${t%/*} GOARCH=$${t#*/} go build -ldflags "-s -w $(LDFLAGS)" -o dist/pgcellar_$${t%/*}_$${t#*/} ./cmd/pgcellar || exit 1; \
		done; \
		(cd dist && shasum -a 256 pgcellar_* > checksums.txt); \
	fi

# The binary must print the version injected at build time.
version-check:
	@v=$$(CGO_ENABLED=0 go run -ldflags "-X main.Version=0.0.0-check" ./cmd/pgcellar version); \
	if [ "$$v" != "pgcellar 0.0.0-check" ]; then echo "unexpected: $$v"; exit 1; fi; echo "$$v"

clean:
	rm -f pgcellar
	rm -rf dist
