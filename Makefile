GO ?= go
BIN := bin

.PHONY: all build test race lint clean run

all: lint test build

build:
	$(GO) build -o $(BIN)/wtd ./cmd/wtd
	$(GO) build -o $(BIN)/wt ./cmd/wt

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 ./...

lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	$(GO) vet ./...

run: build
	$(BIN)/wtd -root $$(pwd) -interval 1s

clean:
	rm -rf $(BIN)
