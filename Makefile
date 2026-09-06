.PHONY: build test test-integration lint install clean verify verify-live

BIN_EXT := $(if $(filter windows,$(shell go env GOOS)),.exe,)

build:
	go build -o bin/pressbooks-pp-cli$(BIN_EXT) ./cmd/pressbooks-pp-cli

test:
	go test ./... -race -count=1

# Runs against live Pressbooks networks, so an endpoint that has moved or gone
# dead fails here instead of shipping.
test-integration:
	go test ./... -tags=integration -count=1

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found; install it from https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	}
	golangci-lint run

# lint is part of verify because CI enforces it: a tree that passes verify
# locally but fails lint in CI is a broken gate, and that is exactly what
# happened on the first public push.
verify: lint
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then printf '%s\n' "$$unformatted"; exit 1; fi
	go vet ./...
	go vet -tags=integration ./...
	go test ./... -race -count=1
	go build ./...

# verify plus the live suite. verify only runs `go vet -tags=integration`,
# which type-checks the integration tests without ever executing them, so a
# tree can pass verify while every live test in it has never once run. Use
# this before shipping, or after touching anything that talks to a host.
# It needs the network and takes a few seconds longer; verify does not.
verify-live: verify
	go test ./... -tags=integration -count=1

install: build
	go install ./cmd/pressbooks-pp-cli
	@mkdir -p $(HOME)/.local/bin
	cp bin/pressbooks-pp-cli$(BIN_EXT) $(HOME)/.local/bin/pressbooks-pp-cli$(BIN_EXT)
	ln -sf pressbooks-pp-cli$(BIN_EXT) $(HOME)/.local/bin/pressbooks-cli$(BIN_EXT)

clean:
	rm -rf bin/ pressbooks-cli pressbooks-pp-cli
