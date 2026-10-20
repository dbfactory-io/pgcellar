VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -X main.Version=$(VERSION)

.PHONY: build test lint run docker clean

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

docker:
	docker build -f deploy/Dockerfile -t pgcellar:dev --build-arg VERSION=$(VERSION) .

clean:
	rm -f pgcellar
	rm -rf dist
