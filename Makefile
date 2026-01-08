# Makefile for grengate

BINARY_NAME=grengate
RELEASE_DIR=./release

.PHONY: all local linux-amd64 linux-arm clean

all: local linux-amd64 linux-arm

local:
	@mkdir -p $(RELEASE_DIR)
	go build -o $(RELEASE_DIR)/$(BINARY_NAME)
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME) for local platform"

linux-amd64:
	@mkdir -p $(RELEASE_DIR)
	GOOS=linux GOARCH=amd64 go build -o $(RELEASE_DIR)/$(BINARY_NAME)_linux_amd64
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME)_linux_amd64"

linux-arm:
	@mkdir -p $(RELEASE_DIR)
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(RELEASE_DIR)/$(BINARY_NAME)_linux_arm
	@echo "Built $(RELEASE_DIR)/$(BINARY_NAME)_linux_arm"

clean:
	rm -rf $(RELEASE_DIR)
	@echo "Cleaned $(RELEASE_DIR)"
