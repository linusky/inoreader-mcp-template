BINARY := inoreader-mcp
BUILD_DIR := bin

.PHONY: build test race vet install clean

build:
	@mkdir -p $(BUILD_DIR)
	go build -buildvcs=false -trimpath -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY) ./cmd/inoreader-mcp

test:
	go test -v ./...

race:
	go test -race -v ./...

vet:
	go vet ./...

install:
	go install -buildvcs=false -trimpath -ldflags="-s -w" ./cmd/inoreader-mcp

clean:
	rm -rf $(BUILD_DIR)
