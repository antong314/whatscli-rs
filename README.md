# whatscli-rs

A terminal WhatsApp client with inline images, on-device translation, and on-device audio transcription.

Split architecture:

- **`backend/`** — Go daemon. Talks to WhatsApp via [whatsmeow](https://github.com/tulir/whatsmeow), runs translation (TranslateGemma 12B via embedded `llama.cpp`) and transcription (Whisper via `whisper.cpp`), and exposes a gRPC API over a Unix domain socket.
- **`tui/`** — Rust [ratatui](https://github.com/ratatui/ratatui) frontend. Connects to the backend over gRPC, renders chats and messages, and displays inline images using the Kitty graphics protocol via [`ratatui-image`](https://crates.io/crates/ratatui-image).
- **`proto/`** — Shared `whatscli.proto` contract.

The split exists so the same backend can later serve a SwiftUI macOS/iOS frontend (or anything else that speaks gRPC) without re-implementing WhatsApp, translation, or transcription.

## Status

This is a personal project. It works on my machine (macOS, Ghostty terminal). It is not packaged for distribution.

## Requirements

- Go 1.26+
- Rust (stable)
- `cmake`, a C/C++ toolchain (for building `llama.cpp`, `whisper.cpp`, `libopus`)
- A terminal with Kitty graphics protocol support (Ghostty, Kitty, WezTerm) for inline images

## Build

```bash
make deps      # one-time: builds vendored llama.cpp + whisper.cpp + opus (slow, ~10 min)
make build     # builds backend/whatscli-server and tui/target/release/whatscli-tui
```

## Run

```bash
make run       # starts the backend daemon, then launches the TUI
```

Or run them separately in two terminals:

```bash
make run-server    # terminal 1
make run-tui       # terminal 2
```

The backend listens on a Unix socket at `$XDG_RUNTIME_DIR/whatscli/whatscli.sock` (falls back to `$TMPDIR/whatscli/whatscli.sock`). On first run, scan the QR code with WhatsApp on your phone.

## Layout

```
whatscli-rs/
├── backend/            # Go daemon
│   ├── cmd/server/     # main entrypoint for whatscli-server
│   ├── messages/       # WhatsApp session, chat/message storage
│   ├── server/         # gRPC server, broadcaster
│   ├── translate/      # TranslateGemma + lingua-go language detection
│   ├── transcribe/     # Whisper wrapper + libopus audio decoding
│   ├── imagerender/    # image cache (used by both old TUI and new server)
│   ├── config/         # XDG-based config and cache paths
│   └── gen/pb/         # generated Go protobuf
├── tui/                # Rust ratatui frontend
│   ├── src/            # main, state (MVU), client (gRPC), media, ui/*
│   ├── tests/          # tmux E2E + ratatui TestBackend snapshot tests
│   └── build.rs        # compiles ../proto/whatscli.proto via tonic-build
└── proto/
    └── whatscli.proto  # shared gRPC contract
```

## Credits

The Go core started as a fork of [normen/whatscli](https://github.com/normen/whatscli) and has been heavily modified — original `tview` UI replaced with a gRPC server, embedded translation and transcription added, plus a new Rust TUI.
