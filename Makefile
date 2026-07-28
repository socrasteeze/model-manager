BINARY := mm
PKG    := ./cmd/mm
BINDIR := bin

# Phase 0 targets. Cross-compilation is a plain GOOS/GOARCH matrix because the
# datastore driver (modernc.org/sqlite) is pure Go -- no cgo, no toolchain per target.
PLATFORMS := linux/amd64 linux/arm64 windows/amd64 darwin/amd64 darwin/arm64

.PHONY: all build test vet fmt clean release

all: vet test build

build:
	@mkdir -p $(BINDIR)
	CGO_ENABLED=0 go build -trimpath -o $(BINDIR)/$(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

release:
	@mkdir -p $(BINDIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -o $(BINDIR)/$(BINARY)-$$os-$$arch$$ext $(PKG) || exit 1; \
	done

clean:
	rm -rf $(BINDIR)
