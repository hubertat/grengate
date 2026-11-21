# Makefile for grengate

BINARY_NAME=grengate
RELEASE_DIR=./release
VERSION ?= $(shell git describe --tags --always --dirty)
GIT_COMMIT := $(shell git rev-parse --short HEAD)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS = -X main.Version=$(VERSION) -X main.GitCommit=$(GIT_COMMIT) -X main.BuildDate=$(BUILD_DATE)
ARM_BINARY=$(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION)_linux_arm

.DEFAULT_GOAL := linux-arm

.PHONY: all local linux-amd64 linux-arm deploy check-deploy clean

all: local linux-amd64 linux-arm

local:
	@mkdir -p $(RELEASE_DIR)
	go build -ldflags "$(LDFLAGS)" -o "$(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION)"
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION) for local platform"

linux-amd64:
	@mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o "$(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION)_linux_amd64"
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION)_linux_amd64"

linux-arm:
	@mkdir -p $(RELEASE_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" -o "$(ARM_BINARY)"
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME)_$(VERSION)_linux_arm"

# Example: make deploy HOST=pi@raspberrypi DEST=/srv/grengate/grengate
deploy: check-deploy
	$(MAKE) linux-arm
	scp "$(ARM_BINARY)" "$(HOST):$(DEST)"

check-deploy:
	@test -n "$(HOST)" || { echo "HOST is required (for example pi@raspberrypi)" >&2; exit 1; }
	@test -n "$(DEST)" || { echo "DEST is required (for example /srv/grengate/grengate)" >&2; exit 1; }

clean:
	rm -rf $(RELEASE_DIR)
	@echo "Cleaned $(RELEASE_DIR)"
