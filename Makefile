.PHONY: all clean clean-data build-rust build-go test run install deploy release \
       build-rust-android build-android-arm64 build-android-x86_64 build-android-lib android

VERSION := 0.7.0
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
	rm -f blocknet
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

# ─── Android ────────────────────────────────────────────────────────────────
# Requires: ANDROID_NDK_HOME, Rust targets (installed automatically),
#           Go with CGO cross-compilation support.
# Usage:
#   make android                          # arm64 (most devices)
#   make build-android-x86_64             # emulators
#   make build-rust-android               # Rust libs only (all arches)

ANDROID_API ?= 21

ifeq ($(OS),darwin)
  NDK_HOST_TAG := darwin-x86_64
else
  NDK_HOST_TAG := linux-x86_64
endif

ifdef ANDROID_NDK_HOME
  NDK_TOOLCHAIN := $(ANDROID_NDK_HOME)/toolchains/llvm/prebuilt/$(NDK_HOST_TAG)/bin
endif

# Build Rust crypto for all Android architectures
build-rust-android:
ifndef ANDROID_NDK_HOME
	$(error Set ANDROID_NDK_HOME to your Android NDK installation)
endif
	@rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android 2>/dev/null || true
	@echo "Building Rust crypto for aarch64-linux-android..."
	@CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER="$(NDK_TOOLCHAIN)/aarch64-linux-android$(ANDROID_API)-clang" \
	 CC_aarch64_linux_android="$(NDK_TOOLCHAIN)/aarch64-linux-android$(ANDROID_API)-clang" \
	 AR_aarch64_linux_android="$(NDK_TOOLCHAIN)/llvm-ar" \
	 cargo build --manifest-path crypto-rs/Cargo.toml --target aarch64-linux-android --release
	@echo "Building Rust crypto for armv7-linux-androideabi..."
	@CARGO_TARGET_ARMV7_LINUX_ANDROIDEABI_LINKER="$(NDK_TOOLCHAIN)/armv7a-linux-androideabi$(ANDROID_API)-clang" \
	 CC_armv7_linux_androideabi="$(NDK_TOOLCHAIN)/armv7a-linux-androideabi$(ANDROID_API)-clang" \
	 AR_armv7_linux_androideabi="$(NDK_TOOLCHAIN)/llvm-ar" \
	 cargo build --manifest-path crypto-rs/Cargo.toml --target armv7-linux-androideabi --release
	@echo "Building Rust crypto for x86_64-linux-android..."
	@CARGO_TARGET_X86_64_LINUX_ANDROID_LINKER="$(NDK_TOOLCHAIN)/x86_64-linux-android$(ANDROID_API)-clang" \
	 CC_x86_64_linux_android="$(NDK_TOOLCHAIN)/x86_64-linux-android$(ANDROID_API)-clang" \
	 AR_x86_64_linux_android="$(NDK_TOOLCHAIN)/llvm-ar" \
	 cargo build --manifest-path crypto-rs/Cargo.toml --target x86_64-linux-android --release

# Build Go binary for Android arm64 (most devices)
build-android-arm64: build-rust-android
	@echo "Staging aarch64 library for CGO..."
	@mkdir -p crypto-rs/target/release
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a ]; then \
	  cp crypto-rs/target/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a.host; \
	fi
	@cp crypto-rs/target/aarch64-linux-android/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a
	@echo "Building Go for android/arm64..."
	@mkdir -p releases
	CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
	CC="$(NDK_TOOLCHAIN)/aarch64-linux-android$(ANDROID_API)-clang" \
	  go build -ldflags="-s -w" -o releases/blocknet-android-arm64 .
	@echo "Built: releases/blocknet-android-arm64"
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a.host ]; then \
	  mv crypto-rs/target/release/libblocknet_crypto.a.host crypto-rs/target/release/libblocknet_crypto.a; \
	fi

# Build Go binary for Android x86_64 (emulators)
build-android-x86_64: build-rust-android
	@echo "Staging x86_64 library for CGO..."
	@mkdir -p crypto-rs/target/release
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a ]; then \
	  cp crypto-rs/target/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a.host; \
	fi
	@cp crypto-rs/target/x86_64-linux-android/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a
	@echo "Building Go for android/amd64..."
	@mkdir -p releases
	CGO_ENABLED=1 GOOS=android GOARCH=amd64 \
	CC="$(NDK_TOOLCHAIN)/x86_64-linux-android$(ANDROID_API)-clang" \
	  go build -ldflags="-s -w" -o releases/blocknet-android-x86_64 .
	@echo "Built: releases/blocknet-android-x86_64"
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a.host ]; then \
	  mv crypto-rs/target/release/libblocknet_crypto.a.host crypto-rs/target/release/libblocknet_crypto.a; \
	fi

# Build as shared library for Android JNI integration (arm64)
build-android-lib: build-rust-android
	@echo "Staging aarch64 library for CGO..."
	@mkdir -p crypto-rs/target/release
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a ]; then \
	  cp crypto-rs/target/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a.host; \
	fi
	@cp crypto-rs/target/aarch64-linux-android/release/libblocknet_crypto.a crypto-rs/target/release/libblocknet_crypto.a
	@echo "Building libblocknet.so for android/arm64..."
	@mkdir -p releases
	CGO_ENABLED=1 GOOS=android GOARCH=arm64 \
	CC="$(NDK_TOOLCHAIN)/aarch64-linux-android$(ANDROID_API)-clang" \
	  go build -buildmode=c-shared -o releases/libblocknet.so .
	@echo "Built: releases/libblocknet.so + releases/libblocknet.h"
	@if [ -f crypto-rs/target/release/libblocknet_crypto.a.host ]; then \
	  mv crypto-rs/target/release/libblocknet_crypto.a.host crypto-rs/target/release/libblocknet_crypto.a; \
	fi

# Convenience: build arm64 (default Android target)
android: build-android-arm64
