.PHONY: all clean clean-data build-rust build-go test run install deploy release android

VERSION := 0.14.0
OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ARCH := $(shell uname -m)
ifeq ($(ARCH),x86_64)
	ARCH := amd64
endif
ifeq ($(ARCH),aarch64)
	ARCH := arm64
endif

all: build-rust build-go

# Build Rust crypto library
build-rust:
	@echo "Building Rust crypto library..."
	cd crypto-rs && cargo build --release

# Build Go binary
build-go: build-rust
	@echo "Building Go binary..."
	CGO_ENABLED=1 go build -o blocknet .

# Build release package for current platform
release: build-rust
	@echo "Building release for $(OS)-$(ARCH)..."
	@mkdir -p releases
	@CGO_ENABLED=1 go build -ldflags="-s -w" -o releases/blocknet .
	@cd releases && zip -q blocknet-$(VERSION)-$(OS)-$(ARCH).zip blocknet
ifeq ($(OS),darwin)
	@cd releases && shasum -a 256 blocknet-$(VERSION)-$(OS)-$(ARCH).zip >> SHA256SUMS.txt
else
	@cd releases && sha256sum blocknet-$(VERSION)-$(OS)-$(ARCH).zip >> SHA256SUMS.txt
endif
	@rm -f releases/blocknet
	@echo "Built: releases/blocknet-$(VERSION)-$(OS)-$(ARCH).zip"
	@echo "Checksum added to releases/SHA256SUMS.txt"

ANDROID_NDK_HOME ?= $(firstword $(wildcard /opt/homebrew/share/android-ndk $(HOME)/Library/Android/sdk/ndk/* $(HOME)/Android/Sdk/ndk/*))
ANDROID_API := 24
ANDROID_CC = $(firstword $(wildcard $(ANDROID_NDK_HOME)/toolchains/llvm/prebuilt/*/bin/aarch64-linux-android$(ANDROID_API)-clang))
ANDROID_AR = $(dir $(ANDROID_CC))llvm-ar

android:
	@test -n "$(ANDROID_CC)" || (echo "Android NDK not found. Install it with: brew install --cask android-ndk" && exit 1)
	@echo "Building for android-arm64..."
	rustup target add aarch64-linux-android
	cd crypto-rs && CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER=$(ANDROID_CC) CC_aarch64_linux_android=$(ANDROID_CC) AR_aarch64_linux_android=$(ANDROID_AR) cargo build --release --target aarch64-linux-android
	CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=$(ANDROID_CC) go build -ldflags="-s -w -checklinkname=0" -o blocknet-core-arm64-android .

# Run tests
test:
	@echo "Testing Rust library..."
	cd crypto-rs && cargo test
	@echo "Testing Go code..."
	go test -v ./...

# Run the project
run: build-go
	./blocknet

# Run as daemon (headless)
daemon: build-go
	./blocknet --daemon

# Run as seed node
seed: build-go
	./blocknet --seed --daemon

# Clean build artifacts
clean:
	@echo "Cleaning..."
	cd crypto-rs && cargo clean
	rm -f blocknet blocknet-core-arm64-android
	rm -rf releases/
	go clean

# Clean chain data (local node state)
clean-data:
	@echo "Removing chain data..."
	rm -rf data/

# Clean everything including wallet
clean-all: clean clean-data
	rm -f wallet.dat

# Install dependencies
deps:
	@echo "Installing Rust dependencies..."
	cd crypto-rs && cargo fetch
	@echo "Installing Go dependencies..."
	go mod download
