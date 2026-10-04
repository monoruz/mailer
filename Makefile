# mailer - minimal SMTP -> Telegram relay
#
#   make              cross-compile for the server (linux/amd64)
#   make local        build for this machine
#   make run          build and run locally (reads .env)
#   make test-upload FILE=x.pdf   send one file to the configured chat
#   make clean

BIN     ?= mailer
GOOS    ?= linux
GOARCH  ?= amd64
LDFLAGS ?= -s -w

# CGO off keeps the binary static, so it runs on any Linux without matching libc.
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

# Same, for an ARM server (Graviton, Ampere, Pi).
arm64:
	$(MAKE) build GOARCH=arm64 BIN=$(BIN)-arm64

local:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

run: local
	./$(BIN)

test-upload: local
	@test -n "$(FILE)" || { echo "usage: make test-upload FILE=path"; exit 1; }
	./$(BIN) -test-upload "$(FILE)"

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o /dev/null .

clean:
	rm -f $(BIN) $(BIN)-arm64

.PHONY: build arm64 local run test-upload fmt vet check clean
