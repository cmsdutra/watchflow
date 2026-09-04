APP_NAME := watchflow
BIN_DIR := bin
BINARY := $(BIN_DIR)/$(APP_NAME)
MAIN_SRC := ./cmd/watchflow

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE ?= $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')

LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) -s -w

.PHONY: all build test lint clean help

all: lint test build

## build: Compila o binário do WatchFlow
build:
	@mkdir -p $(BIN_DIR)
	@echo "==> Compilando $(BINARY) (v$(VERSION))..."
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(MAIN_SRC)
	@echo "==> Binário gerado em $(BINARY)"

## test: Executa a suíte de testes unitários com detecção de corrida (-race)
test:
	@echo "==> Executando testes unitários..."
	go test -v -race ./...

## lint: Executa o golangci-lint sobre todos os pacotes
lint:
	@echo "==> Executando golangci-lint..."
	golangci-lint run ./...

## clean: Remove binários compilados e artefatos temporários
clean:
	@echo "==> Limpando artefatos de build..."
	rm -rf $(BIN_DIR) coverage.out coverage.html *.tmp

## help: Exibe esta mensagem de ajuda
help:
	@echo "Comandos disponíveis:"
	@sed -n 's/^##//p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
