
# Build
VERSION=`git describe --tags`
BUILD=`date +%FT%T%z`

# Binary names
BINARY_NAME=grengate
BINARY_linux=$(BINARY_NAME)_linux_$(VERSION)
BINARY_darwin=$(BINARY_NAME)_darwin_$(VERSION)

# Ld
LDFLAGS=-ldflags "-w -s -X main.Version=${VERSION} -X main.Build=${BUILD}"

# Basic go commands
GOCMD=go
GOBUILD=$(GOCMD) build $(LDFLAGS)


all: build-linux build-darwin pack-static

build:
	$(GOBUILD) -o $(BINARY_NAME) -v


build-linux:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o ./releases/$(BINARY_linux) -v

build-darwin:
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -o ./releases/$(BINARY_darwin) -v

pack-static:
	tar -czvf ./releases/static_$(BINARY_NAME)_$(VERSION).tar.gz ./static

run-app:
	./$(BINARY_NAME)

run: build run-app
