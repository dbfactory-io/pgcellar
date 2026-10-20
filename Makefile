VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test lint run demo docker release-dry version-check clean docs docs-pdf og

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

# The manual (docs/manual/*.md) as static HTML in site/docs/, plus site/sitemap.xml.
# The generator is a separate Go module (tools/docs) so the product gets no extra dependency.
docs:
	cd tools/docs && go run . -root "$(CURDIR)"

# The same manual as one PDF, printed by headless Chrome from site/docs/_manual.html.
# Without Chrome or Chromium the target says so and stops without failing the build.
docs-pdf: docs
	@chrome=""; \
	for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" google-chrome google-chrome-stable chromium chromium-browser; do \
		if [ -x "$$c" ] || command -v "$$c" >/dev/null 2>&1; then chrome="$$c"; break; fi; \
	done; \
	if [ -z "$$chrome" ]; then echo "docs-pdf: Chrome or Chromium not found, PDF not produced (the HTML manual in site/docs/ is built)"; exit 0; fi; \
	"$$chrome" --headless=new --disable-gpu --no-pdf-header-footer \
		--print-to-pdf="$(CURDIR)/site/docs/pgcellar-manual.pdf" \
		"file://$(CURDIR)/site/docs/_manual.html" >/dev/null 2>&1 \
	&& echo "wrote site/docs/pgcellar-manual.pdf" \
	|| { echo "docs-pdf: Chrome failed to print the PDF"; exit 1; }

# Social card (1200x630) rendered by headless Chrome from tools/og/og.html.
# Without Chrome or Chromium the target says so and stops without failing the build.
og:
	@chrome=""; \
	for c in "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" google-chrome google-chrome-stable chromium chromium-browser; do \
		if [ -x "$$c" ] || command -v "$$c" >/dev/null 2>&1; then chrome="$$c"; break; fi; \
	done; \
	if [ -z "$$chrome" ]; then echo "og: Chrome or Chromium not found, site/og.png not produced"; exit 0; fi; \
	"$$chrome" --headless=new --disable-gpu --hide-scrollbars --window-size=1200,630 \
		--screenshot="$(CURDIR)/site/og.png" \
		"file://$(CURDIR)/tools/og/og.html" >/dev/null 2>&1 \
	&& echo "wrote site/og.png" \
	|| { echo "og: Chrome failed to render the card"; exit 1; }

clean:
	rm -f pgcellar
	rm -rf dist
