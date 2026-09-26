# All builds are fully static: CGO_ENABLED=0 makes Go use its own DNS
# resolver instead of glibc's, and the SQLite driver is pure Go, so the
# binaries have no libc dependency and run on any Linux of the same arch.
export CGO_ENABLED := 0

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
GOFLAGS := -trimpath -ldflags='$(LDFLAGS)'
CMDS    := gsactiond gsprov

# Targets for "make dist". GOARM=7 is used for linux/arm.
PLATFORMS ?= linux/amd64 linux/arm64 linux/arm linux/386

.PHONY: build test dist clean

build:
	go build $(GOFLAGS) -o bin/ ./cmd/...

test:
	go test ./...

dist:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/$(VERSION)/$$os-$$arch; \
		mkdir -p $$out; \
		for c in $(CMDS); do \
			echo "build $$out/$$c"; \
			GOOS=$$os GOARCH=$$arch GOARM=7 go build $(GOFLAGS) -o $$out/$$c ./cmd/$$c || exit 1; \
		done; \
	done

clean:
	rm -rf bin dist
