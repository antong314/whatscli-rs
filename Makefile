# Top-level Makefile for whatscli-rs
#
# Project layout:
#   backend/  - Go daemon (whatsmeow + translation + transcription, gRPC server)
#   tui/      - Rust ratatui frontend (gRPC client)
#   proto/    - Shared whatscli.proto contract
#
# Common workflows:
#   make deps         - Build vendored native dependencies (llama.cpp, whisper.cpp, opus)
#   make build        - Build both backend and TUI binaries
#   make run          - Start the backend in the background, then launch the TUI
#   make run-server   - Start only the backend (foreground)
#   make run-tui      - Start only the TUI (assumes backend is already running)
#   make clean        - Remove build artifacts from both subprojects

.PHONY: build build-server build-tui run run-server run-tui deps clean

build: build-server build-tui

build-server:
	$(MAKE) -C backend build-server

build-tui:
	cd tui && cargo build --release

deps:
	$(MAKE) -C backend deps

run: run-server
	@sleep 2
	cd tui && cargo run --release

run-server:
	$(MAKE) -C backend run-server &

run-tui:
	cd tui && cargo run --release

clean:
	$(MAKE) -C backend clean
	cd tui && cargo clean
