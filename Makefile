.PHONY: build run clean deps

BINARY_NAME=niulai
BUILD_DIR=./build

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/niulai

run:
	go run ./cmd/niulai

deps:
	go mod download
	go mod tidy

clean:
	rm -rf $(BUILD_DIR)

# 交叉编译 Linux 版本
build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./cmd/niulai

# 交叉编译 macOS 版本（Intel）
build-darwin-amd64:
	GOOS=darwin GOARCH=amd64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-amd64 ./cmd/niulai

# 交叉编译 macOS 版本（Apple Silicon）
build-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./cmd/niulai

# 编译全部目标平台
build-all: build-linux build-darwin-amd64 build-darwin-arm64
