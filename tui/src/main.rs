use std::path::PathBuf;
use std::time::Duration;

use color_eyre::Result;
use crossterm::event::{
    self, Event, KeyCode, KeyEventKind, KeyModifiers, KeyboardEnhancementFlags,
    PopKeyboardEnhancementFlags, PushKeyboardEnhancementFlags,
};
use futures::StreamExt;
use ratatui::DefaultTerminal;

use whatscli_tui::client;
use whatscli_tui::media;
use whatscli_tui::proto::whatscli::{self as pb, ClientMessage};
use whatscli_tui::state::{self, App, ConnectionState, FocusPane, InputMode};
use whatscli_tui::ui;

fn socket_path() -> PathBuf {
    if let Ok(xdg) = std::env::var("XDG_RUNTIME_DIR") {
        return PathBuf::from(xdg).join("whatscli").join("whatscli.sock");
    }
    std::env::temp_dir().join("whatscli").join("whatscli.sock")
}

fn pid_path() -> PathBuf {
    if let Ok(xdg) = std::env::var("XDG_RUNTIME_DIR") {
        return PathBuf::from(xdg).join("whatscli").join("whatscli.pid");
    }
    std::env::temp_dir().join("whatscli").join("whatscli.pid")
}

fn is_backend_running() -> bool {
    if let Ok(pid_str) = std::fs::read_to_string(pid_path()) {
        if let Ok(pid) = pid_str.trim().parse::<i32>() {
            unsafe {
                return libc::kill(pid, 0) == 0;
            }
        }
    }
    false
}

fn server_binary_path() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    let dir = exe.parent()?;
    let server = dir.join("whatscli-server");
    if server.exists() {
        return Some(server);
    }
    let workspace_server = dir
        .parent()
        .and_then(|d| d.parent())
        .map(|d| d.join("whatscli").join("whatscli-server"));
    workspace_server.filter(|p| p.exists())
}

fn auto_start_backend() -> Option<std::process::Child> {
    if is_backend_running() {
        return None;
    }

    let server_path = server_binary_path()?;
    std::process::Command::new(server_path)
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn()
        .ok()
}

#[tokio::main]
async fn main() -> Result<()> {
    color_eyre::install()?;

    let mut app = App::new();
    app.image_cache.init_picker();
    let sock = socket_path();

    let _backend = auto_start_backend();
    if _backend.is_some() {
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    let connect_result = client::connect(sock.clone()).await;
    let (client_handle, mut grpc_rx) = match connect_result {
        Ok((h, rx)) => {
            app.connection_state = ConnectionState::Connected;
            (Some(h), Some(rx))
        }
        Err(_) => {
            tokio::time::sleep(Duration::from_secs(1)).await;
            match client::connect(sock.clone()).await {
                Ok((h, rx)) => {
                    app.connection_state = ConnectionState::Connected;
                    (Some(h), Some(rx))
                }
                Err(e) => {
                    eprintln!("Failed to connect to backend: {}. Start whatscli-server first.", e);
                    app.connection_state = ConnectionState::Disconnected;
                    (None, None)
                }
            }
        }
    };

    let mut terminal = ratatui::init();

    // Try to enable the kitty keyboard protocol so we can distinguish
    // Shift+Enter (newline in composer) from plain Enter (send). Terminals that
    // don't support it just ignore the escape sequence.
    let kbd_enhanced = crossterm::execute!(
        std::io::stdout(),
        PushKeyboardEnhancementFlags(
            KeyboardEnhancementFlags::DISAMBIGUATE_ESCAPE_CODES
                | KeyboardEnhancementFlags::REPORT_EVENT_TYPES
        )
    )
    .is_ok();

    let result = run_app(&mut terminal, &mut app, client_handle.as_ref(), &mut grpc_rx).await;

    if kbd_enhanced {
        let _ = crossterm::execute!(std::io::stdout(), PopKeyboardEnhancementFlags);
    }
    ratatui::restore();
    result
}

async fn run_app(
    terminal: &mut DefaultTerminal,
    app: &mut App,
    client_handle: Option<&client::ClientHandle>,
    grpc_rx: &mut Option<tokio::sync::mpsc::Receiver<pb::ServerEvent>>,
) -> Result<()> {
    let mut event_reader = crossterm::event::EventStream::new();
    let tick_rate = Duration::from_millis(250);

    let (img_request_tx, mut img_response_rx) = media::spawn_media_fetcher(socket_path());

    while app.running {
        request_visible_images(app, &img_request_tx);

        // Drop expired transient toasts so the status bar reverts to its
        // normal connection / chat name display once the TTL elapses.
        // Cheap (just a wall-clock comparison), so safe to call every loop.
        app.tick_toast();

        terminal.draw(|f| ui::draw(f, app))?;

        let grpc_event = async {
            if let Some(rx) = grpc_rx.as_mut() {
                rx.recv().await
            } else {
                std::future::pending().await
            }
        };

        tokio::select! {
            Some(term_event) = event_reader.next() => {
                if let Ok(evt) = term_event {
                    if let Some(cmd) = handle_terminal_event(app, evt) {
                        if let Some(h) = client_handle {
                            h.send(cmd);
                        }
                    }
                }
            }
            Some(server_event) = grpc_event => {
                app.handle_server_event(server_event);
                while let Some(rx) = grpc_rx.as_mut() {
                    match rx.try_recv() {
                        Ok(evt) => app.handle_server_event(evt),
                        Err(_) => break,
                    }
                }
            }
            Some(img_resp) = img_response_rx.recv() => {
                app.image_cache.handle_response(img_resp.message_id, img_resp.image);
            }
            _ = tokio::time::sleep(tick_rate) => {}
        }
    }

    Ok(())
}

fn request_visible_images(
    app: &mut App,
    tx: &tokio::sync::mpsc::UnboundedSender<String>,
) {
    if !app.image_cache.has_graphics_support() {
        return;
    }
    if app.image_cache.loading_count() >= 3 {
        return;
    }

    let total = app.messages.len();
    if total == 0 {
        return;
    }

    // Request images for the last ~60 messages (most recent), iterating from
    // newest to oldest so the most recent images load first.
    let start = total.saturating_sub(60);
    let mut requested = 0;

    for msg in app.messages[start..].iter().rev() {
        if requested >= 3 {
            break;
        }
        let kind = pb::MessageKind::try_from(msg.kind).unwrap_or(pb::MessageKind::Text);
        if kind == pb::MessageKind::Image
            && !app.image_cache.contains(&msg.id)
            && !app.image_cache.is_loading(&msg.id)
            && !app.image_cache.is_failed(&msg.id)
        {
            app.image_cache.mark_loading(&msg.id);
            let _ = tx.send(msg.id.clone());
            requested += 1;
        }
    }
}

fn handle_terminal_event(app: &mut App, evt: Event) -> Option<ClientMessage> {
    match evt {
        Event::Key(key) => {
            // With kitty keyboard protocol enabled we also get Repeat and
            // Release events. Only react to Press to keep behavior consistent
            // with terminals that don't report event types.
            if !matches!(key.kind, KeyEventKind::Press) {
                return None;
            }
            handle_key(app, key)
        }
        Event::Resize(cols, rows) => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::Handshake(pb::ConnectHandshake {
                viewport_rows: rows as i32,
                viewport_cols: cols as i32,
                client_version: env!("CARGO_PKG_VERSION").to_string(),
            })),
        }),
        _ => None,
    }
}

fn handle_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    // Modal help popup: while open, swallow every keystroke. Esc, ?, q, F1
    // close it; arrow keys / PgUp / PgDn scroll within it; everything else
    // is a no-op so users can't accidentally send a message or pick a chat
    // by mashing keys.
    if app.show_help {
        let ctrl_q = key.modifiers.contains(KeyModifiers::CONTROL)
            && matches!(key.code, KeyCode::Char('q'));
        if ctrl_q {
            app.running = false;
            return None;
        }
        match key.code {
            KeyCode::Esc
            | KeyCode::F(1)
            | KeyCode::Char('?')
            | KeyCode::Char('q') => {
                app.close_help();
            }
            KeyCode::Up | KeyCode::Char('k') => app.help_scroll_up(1),
            KeyCode::Down | KeyCode::Char('j') => app.help_scroll_down(1),
            KeyCode::PageUp => app.help_scroll_up(10),
            KeyCode::PageDown | KeyCode::Char(' ') => app.help_scroll_down(10),
            KeyCode::Home | KeyCode::Char('g') => app.help_scroll = 0,
            KeyCode::End | KeyCode::Char('G') => app.help_scroll = u16::MAX,
            _ => {}
        }
        return None;
    }

    if key.modifiers.contains(KeyModifiers::CONTROL) {
        match key.code {
            KeyCode::Char('q') => {
                app.running = false;
                return None;
            }
            KeyCode::Char('u') => {
                if app.current_chat.is_some() {
                    return Some(ClientMessage {
                        msg: Some(pb::client_message::Msg::MarkUnread(pb::MarkUnread {
                            chat_id: app.current_chat.clone().unwrap_or_default(),
                        })),
                    });
                }
                return None;
            }
            KeyCode::Char('n') => {
                navigate_chat(app, 1);
                return select_current_chat(app, SelectIntent::Probe);
            }
            KeyCode::Char('p') => {
                navigate_chat(app, -1);
                return select_current_chat(app, SelectIntent::Probe);
            }
            KeyCode::Char('b') => {
                return Some(ClientMessage {
                    msg: Some(pb::client_message::Msg::RequestBacklog(pb::RequestBacklog {})),
                });
            }
            KeyCode::Char('f') => {
                app.input_mode = InputMode::Search;
                app.input_buffer.clear();
                return None;
            }
            KeyCode::Char('i') => {
                // Toggle the "show unread only" view. Useful for triage when
                // you want to focus on the inbox.
                app.toggle_unread_only();
                return None;
            }
            _ => {}
        }
    }

    // Help popup triggers. F1 always opens; '?' opens only when it would not
    // be ambiguous with text input - i.e. composer is empty AND we're not in
    // a modal slash-command/search input.
    if matches!(key.code, KeyCode::F(1)) {
        app.toggle_help();
        return None;
    }
    if matches!(key.code, KeyCode::Char('?'))
        && app.input_mode == InputMode::Normal
        && app.composer.is_empty()
        && app.chat_filter.is_empty()
    {
        app.toggle_help();
        return None;
    }

    match app.input_mode {
        InputMode::Normal => handle_normal_key(app, key),
        InputMode::Command | InputMode::Search => handle_input_key(app, key),
    }
}

fn handle_normal_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    // App-level keys that are never forwarded to the composer.
    match key.code {
        KeyCode::Tab => {
            app.focus_next();
            return None;
        }
        KeyCode::PageUp => {
            app.scroll_up(10);
            return None;
        }
        KeyCode::PageDown => {
            app.scroll_down(10);
            return None;
        }
        KeyCode::Esc => {
            if app.focus_is_chat_list() && !app.chat_filter.is_empty() {
                app.chat_filter.clear();
                app.chat_list_index = 0;
                return select_current_chat(app, SelectIntent::Probe);
            }
            return None;
        }
        _ => {}
    }

    // Chat-list-focused navigation and live filtering.
    if app.focus_is_chat_list() {
        return handle_chat_list_key(app, key);
    }

    // Messages-pane-focused: forward to the composer, with overrides.
    handle_composer_key(app, key)
}

fn handle_chat_list_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    match key.code {
        KeyCode::Up => {
            navigate_chat(app, -1);
            select_current_chat(app, SelectIntent::Probe)
        }
        KeyCode::Down => {
            navigate_chat(app, 1);
            select_current_chat(app, SelectIntent::Probe)
        }
        KeyCode::Enter => {
            if app.is_archived_folder_selected() {
                app.show_archived = !app.show_archived;
                return None;
            }
            // Enter on a row is a strong "I want this chat" signal: load the
            // content AND mark it read immediately. Arrow-key navigation, by
            // contrast, only probes (the backend defers the read receipt).
            let cmd = select_current_chat(app, SelectIntent::Commit);
            // Picking a chat means "I'm ready to read/reply": clear any active
            // filter so a future Tab back to the list shows the full list, and
            // shift focus to the composer so the next keystroke is text input
            // rather than a filter character.
            app.chat_filter.clear();
            app.chat_list_index = 0;
            app.focus = FocusPane::Messages;
            cmd
        }
        KeyCode::Char(c) => {
            // Special case: `/` in an empty filter context jumps into Command mode
            // (this matches the legacy whatscli behavior).
            if c == '/' && app.chat_filter.is_empty() {
                app.input_mode = InputMode::Command;
                app.input_buffer.clear();
                return None;
            }
            app.chat_filter.push(c);
            app.chat_list_index = 0;
            select_current_chat(app, SelectIntent::Probe)
        }
        KeyCode::Backspace => {
            if !app.chat_filter.is_empty() {
                app.chat_filter.pop();
                app.chat_list_index = 0;
                return select_current_chat(app, SelectIntent::Probe);
            }
            None
        }
        _ => None,
    }
}

fn handle_composer_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    let shift = key.modifiers.contains(KeyModifiers::SHIFT);
    let composer_empty = app.composer.is_empty();

    // Pending revoke confirmation: any keystroke other than y/Y cancels.
    // Handled at the very top so it can never be swallowed by Enter / Esc /
    // letter-shortcut paths below.
    if app.pending_revoke.is_some() {
        return resolve_pending_revoke(app, key);
    }

    // Single-letter attachment shortcuts. Only active when the composer is
    // empty so they can't shadow normal text input. Move this *before* the
    // Enter/arrow handling so an empty-composer `o` doesn't get swallowed
    // as text-area input.
    if composer_empty && !key.modifiers.contains(KeyModifiers::CONTROL) {
        if let Some(cmd) = handle_attachment_shortcut(app, key) {
            return cmd;
        }
        // `handle_attachment_shortcut` returns Some(_) only when it claimed
        // the key. Falling through here means the keystroke is unrelated to
        // attachments and we should keep processing it normally.
    }

    // Enter sends the message; Shift+Enter inserts a newline.
    if let KeyCode::Enter = key.code {
        if shift {
            app.composer.insert_newline();
            return None;
        }
        return send_composer(app);
    }

    // Up/Down with an empty composer drives the message cursor. The cursor
    // walks one message at a time; only when it's already at the newest /
    // oldest message does the arrow fall through to scrolling history.
    // With a non-empty composer we keep the legacy Slack-style behaviour:
    // single-line draft → scroll, multi-line → move cursor within textarea.
    if matches!(key.code, KeyCode::Up | KeyCode::Down) && !shift {
        if composer_empty {
            return handle_message_pane_arrow(app, key.code);
        }

        let (cursor_row, _) = app.composer.cursor();
        let total_rows = app.composer.lines().len();
        let last_row = total_rows.saturating_sub(1);

        match key.code {
            KeyCode::Up if cursor_row == 0 => {
                app.scroll_up(1);
                return None;
            }
            KeyCode::Down if cursor_row >= last_row => {
                app.scroll_down(1);
                return None;
            }
            _ => {}
        }
    }

    // Forward everything else to the textarea, which handles arrows, Home/End,
    // Ctrl+A/E, Ctrl+W (delete word), word jumps, Backspace/Delete, and printable
    // chars. tui-textarea consumes a crossterm KeyEvent directly.
    app.composer.input(key);
    None
}

/// Up/Down on an empty composer: move the in-pane message cursor; only
/// when already at the edge does the arrow fall through to a one-row
/// scroll of message history. This matches the user's mental model of
/// "this arrow picks the next/previous message" rather than "this arrow
/// nudges the viewport".
fn handle_message_pane_arrow(app: &mut App, code: KeyCode) -> Option<ClientMessage> {
    let delta = if matches!(code, KeyCode::Up) { -1 } else { 1 };

    // No messages → nothing to put a cursor on. Preserve the legacy
    // "arrow scrolls history" behaviour for empty / never-loaded chats so
    // the user can still page up to wait for the backlog.
    if app.messages.is_empty() {
        if delta < 0 {
            app.scroll_up(1);
        } else {
            app.scroll_down(1);
        }
        return None;
    }

    // First arrow press initialises the cursor — the cursor goes to the
    // newest visible message on the very first Up (so subsequent Ups walk
    // back through history) or to the oldest on the first Down.
    if app.message_cursor.is_none() {
        let _ = app.navigate_message(delta);
        return None;
    }

    let cursor = app.message_cursor.unwrap_or(0);
    let last = app.messages.len() - 1;

    let at_edge = (delta < 0 && cursor == 0) || (delta > 0 && cursor == last);
    if at_edge {
        // Pressing past the end falls through to one row of scroll so the
        // user can still inspect history above / page below the latest.
        if delta < 0 {
            app.scroll_up(1);
        } else {
            app.scroll_down(1);
        }
        return None;
    }

    let _ = app.navigate_message(delta);
    None
}

/// Empty-composer single-letter message-cursor shortcuts:
///
/// * `s` — save the cursored message's attachment (download to
///   `~/Downloads`, no auto-open).
/// * `o` — open the attachment (download then ask the OS to open it).
/// * `d` — delete (revoke) the cursored message. Two-step: arms a
///   confirmation; the next `y`/`Y` confirms, anything else cancels.
/// * `t` — force-translate the cursored message, bypassing our
///   per-message translation classifier and any cached result.
///   Used when the auto-translation got it wrong (typically a
///   non-English message in a thread that the classifier mistook
///   for English). Only valid on text messages with non-empty text.
/// * `c` — copy the cursored message's text to the clipboard. Prefers
///   the transcript (the point: long voice-note transcripts are a pain
///   to select by hand), then a translation, then the message body.
///
/// Returns `Some(cmd)` if the key was claimed *and* produced a server
/// command, `Some(None-equivalent)` via early-return when claimed without
/// a command (e.g. arming a confirmation, or showing an error toast),
/// or `None` when the key is not an attachment shortcut and should fall
/// through to the rest of the composer pipeline.
fn handle_attachment_shortcut(
    app: &mut App,
    key: event::KeyEvent,
) -> Option<Option<ClientMessage>> {
    let c = match key.code {
        KeyCode::Char(c) if matches!(c, 's' | 'o' | 'd' | 't' | 'c') => c,
        _ => return None,
    };

    // Resolve the target message: explicit cursor wins; otherwise fall
    // back to the most recent message in view (the user's "the file I
    // just saw" intent).
    let msg_id = match app.cursored_message() {
        Some(m) => Some(m.id.clone()),
        None => app.messages.last().map(|m| m.id.clone()),
    };
    let Some(msg_id) = msg_id else {
        app.show_toast("No messages here yet".into(), state::ToastLevel::Info);
        return Some(None);
    };

    let msg = match app.messages.iter().find(|m| m.id == msg_id) {
        Some(m) => m,
        None => return Some(None),
    };

    // Per-key applicability: 's'/'o' need an attachment, 't' needs text,
    // 'd' works on anything we received.
    match c {
        's' | 'o' if !is_downloadable(msg) => {
            app.show_toast(
                "No attachment on this message".into(),
                state::ToastLevel::Info,
            );
            return Some(None);
        }
        'c' if copyable_content(app, msg).is_none() => {
            app.show_toast(
                "Nothing to copy on this message".into(),
                state::ToastLevel::Info,
            );
            return Some(None);
        }
        't' if msg.text.trim().is_empty() => {
            app.show_toast(
                "No text on this message to translate".into(),
                state::ToastLevel::Info,
            );
            return Some(None);
        }
        _ => {}
    }

    match c {
        's' => {
            app.show_toast("Saving…".into(), state::ToastLevel::Info);
            Some(Some(ClientMessage {
                msg: Some(pb::client_message::Msg::DownloadMedia(pb::DownloadMedia {
                    message_id: msg_id,
                })),
            }))
        }
        'o' => {
            app.show_toast("Opening…".into(), state::ToastLevel::Info);
            Some(Some(ClientMessage {
                msg: Some(pb::client_message::Msg::OpenMedia(pb::OpenMedia {
                    message_id: msg_id,
                })),
            }))
        }
        'd' => {
            app.pending_revoke = Some(msg_id);
            app.show_toast(
                "Delete this message? Press y to confirm, any other key to cancel."
                    .into(),
                state::ToastLevel::Info,
            );
            Some(None)
        }
        't' => {
            app.show_toast("Translating…".into(), state::ToastLevel::Info);
            Some(Some(ClientMessage {
                msg: Some(pb::client_message::Msg::ForceTranslate(pb::ForceTranslate {
                    message_id: msg_id,
                })),
            }))
        }
        'c' => {
            // `copyable_content` already vetted (applicability guard above),
            // so `unwrap_or_default` only guards against a TOCTOU-style empty
            // and never actually fires here.
            let content = copyable_content(app, msg).unwrap_or_default();
            match copy_to_clipboard(&content) {
                Ok(()) => app.show_toast(
                    "Copied to clipboard".into(),
                    state::ToastLevel::Success,
                ),
                Err(e) => app.show_toast(
                    format!("Copy failed: {e}"),
                    state::ToastLevel::Error,
                ),
            }
            Some(None)
        }
        _ => unreachable!(),
    }
}

/// Resolve what pressing `c` copies for a message. Transcript wins — that's the
/// whole reason this exists, since a multi-minute voice note produces a wall of
/// text that's miserable to select with a mouse. Falls back to a translation,
/// then the message's own text. Returns `None` when there's nothing worth
/// copying (e.g. a bare image with no caption, transcript, or translation).
fn copyable_content(app: &App, msg: &pb::MessageProto) -> Option<String> {
    if let Some(tr) = app.transcriptions.get(&msg.id)
        && !tr.trim().is_empty()
    {
        return Some(tr.clone());
    }
    if let Some(tl) = app.translations.get(&msg.id)
        && !tl.trim().is_empty()
    {
        return Some(tl.clone());
    }
    if !msg.text.trim().is_empty() {
        return Some(msg.text.clone());
    }
    None
}

/// Put `text` on the system clipboard. Each call opens a fresh `arboard`
/// handle; on macOS/Windows that hands ownership to the OS pasteboard, so the
/// text survives the handle being dropped. Errors are surfaced to the user as
/// a toast rather than swallowed, since a silent failed copy is worse than a
/// visible one.
fn copy_to_clipboard(text: &str) -> Result<(), String> {
    let mut clipboard = arboard::Clipboard::new().map_err(|e| e.to_string())?;
    clipboard.set_text(text.to_string()).map_err(|e| e.to_string())
}

/// Resolve a previously-armed `d` (revoke) confirmation. `y`/`Y` fires
/// the revoke; anything else cancels and shows a brief toast so the user
/// knows what happened.
fn resolve_pending_revoke(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    let msg_id = app.pending_revoke.take()?;
    match key.code {
        KeyCode::Char('y') | KeyCode::Char('Y') => {
            app.show_toast("Deleting…".into(), state::ToastLevel::Info);
            Some(ClientMessage {
                msg: Some(pb::client_message::Msg::Revoke(pb::RevokeMessage {
                    message_id: msg_id,
                })),
            })
        }
        _ => {
            app.show_toast("Delete cancelled".into(), state::ToastLevel::Info);
            None
        }
    }
}

/// Whether a message has a downloadable attachment (image / video / audio
/// / document). Text and unknown messages don't.
fn is_downloadable(msg: &pb::MessageProto) -> bool {
    matches!(
        pb::MessageKind::try_from(msg.kind).unwrap_or(pb::MessageKind::Text),
        pb::MessageKind::Image
            | pb::MessageKind::Video
            | pb::MessageKind::Audio
            | pb::MessageKind::Document
    )
}

fn send_composer(app: &mut App) -> Option<ClientMessage> {
    let text = app.composer_text();
    if text.is_empty() {
        return None;
    }
    let chat_id = app.current_chat.clone()?;
    app.composer_reset();
    if let Some(rest) = text.strip_prefix('/') {
        return parse_command(app, rest);
    }
    Some(ClientMessage {
        msg: Some(pb::client_message::Msg::SendText(pb::SendText {
            chat_id,
            text,
        })),
    })
}

fn handle_input_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    match key.code {
        KeyCode::Esc => {
            app.input_mode = InputMode::Normal;
            app.input_buffer.clear();
            None
        }
        KeyCode::Enter => {
            let text = std::mem::take(&mut app.input_buffer);
            let was_search = app.input_mode == InputMode::Search;
            app.input_mode = InputMode::Normal;

            if was_search {
                return None;
            }

            if !text.is_empty() {
                return parse_command(app, &text);
            }
            None
        }
        KeyCode::Char(c) => {
            app.input_buffer.push(c);
            None
        }
        KeyCode::Backspace => {
            app.input_buffer.pop();
            None
        }
        _ => None,
    }
}

fn parse_command(app: &App, cmd: &str) -> Option<ClientMessage> {
    let parts: Vec<&str> = cmd.splitn(2, ' ').collect();
    let name = parts[0];
    let param = parts.get(1).copied().unwrap_or("");

    match name {
        "backlog" | "more" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::RequestBacklog(pb::RequestBacklog {})),
        }),
        "read" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::MarkRead(pb::MarkRead {
                chat_id: app.current_chat.clone().unwrap_or_default(),
            })),
        }),
        "unread" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::MarkUnread(pb::MarkUnread {
                chat_id: app.current_chat.clone().unwrap_or_default(),
            })),
        }),
        "login" | "connect" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::Login(pb::LoginRequest {})),
        }),
        "disconnect" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::Disconnect(pb::DisconnectRequest {})),
        }),
        "logout" | "reset" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::Logout(pb::LogoutRequest {})),
        }),
        "download" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::DownloadMedia(pb::DownloadMedia {
                message_id: param.to_string(),
            })),
        }),
        "open" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::OpenMedia(pb::OpenMedia {
                message_id: param.to_string(),
            })),
        }),
        "upload" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::SendMedia(pb::SendMedia {
                chat_id: app.current_chat.clone().unwrap_or_default(),
                file_path: param.to_string(),
                kind: pb::MessageKind::Document as i32,
            })),
        }),
        "leave" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::LeaveGroup(pb::LeaveGroup {})),
        }),
        "info" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::GetInfo(pb::GetInfo {
                message_id: param.to_string(),
            })),
        }),
        "url" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::GetUrl(pb::GetUrl {
                message_id: param.to_string(),
            })),
        }),
        "revoke" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::Revoke(pb::RevokeMessage {
                message_id: param.to_string(),
            })),
        }),
        "subject" => Some(ClientMessage {
            msg: Some(pb::client_message::Msg::SetSubject(pb::SetSubject {
                subject: param.to_string(),
            })),
        }),
        _ => None,
    }
}

fn navigate_chat(app: &mut App, delta: i32) {
    let total = app.visible_chat_rows().len();
    if total == 0 {
        return;
    }
    let new = (app.chat_list_index as i32 + delta).clamp(0, total as i32 - 1) as usize;
    app.chat_list_index = new;
}

/// How strongly the user "wants" the chat we're about to select.
///
/// `Probe` is for keyboard navigation - arrow keys, Ctrl+N/P, live filter
/// typing - where we want to load the chat content for preview but defer
/// the read receipt in case the user is just passing through. The backend
/// uses this to start a short dwell timer.
///
/// `Commit` is for explicit "I want this chat" gestures: pressing Enter on
/// a row, which also moves focus into the composer. The backend marks read
/// immediately.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SelectIntent {
    Probe,
    Commit,
}

fn select_current_chat(app: &mut App, intent: SelectIntent) -> Option<ClientMessage> {
    let chat_id = app.chat_id_at_index(app.chat_list_index)?;
    app.current_chat = Some(chat_id.clone());
    app.messages.clear();
    app.scroll_offset = usize::MAX;
    app.follow_tail = true;
    app.image_cache.clear();
    let intent_pb = match intent {
        SelectIntent::Probe => pb::select_chat::Intent::Probe,
        SelectIntent::Commit => pb::select_chat::Intent::Commit,
    };
    Some(ClientMessage {
        msg: Some(pb::client_message::Msg::SelectChat(pb::SelectChat {
            chat_id,
            intent: intent_pb as i32,
        })),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crossterm::event::{KeyCode, KeyEvent, KeyModifiers};

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent::new(code, KeyModifiers::NONE)
    }

    #[test]
    fn up_arrow_in_empty_composer_with_no_messages_scrolls() {
        // No messages → no message cursor possible, so Up still falls
        // through to scrolling history (preserves the legacy behaviour
        // for empty / never-loaded chats).
        let mut app = App::default();
        app.scroll_offset = 5;
        app.follow_tail = true;

        handle_composer_key(&mut app, key(KeyCode::Up));

        assert_eq!(app.scroll_offset, 4, "scroll_offset should decrease by 1");
        assert!(!app.follow_tail, "scroll_up disengages follow_tail");
        assert_eq!(
            app.composer_text(),
            "",
            "composer should remain unmodified by Up"
        );
    }

    #[test]
    fn down_arrow_in_empty_composer_with_no_messages_scrolls() {
        let mut app = App::default();
        app.scroll_offset = 5;

        handle_composer_key(&mut app, key(KeyCode::Down));

        assert_eq!(app.scroll_offset, 6, "scroll_offset should increase by 1");
    }

    #[test]
    fn up_arrow_in_empty_composer_with_messages_moves_cursor_to_newest() {
        // The interesting new behaviour: with messages present, the first
        // Up press parks the message cursor on the newest message instead
        // of scrolling. Subsequent Ups walk back through history.
        let mut app = App::default();
        app.scroll_offset = 5;
        app.follow_tail = true;
        app.messages = vec![
            test_message("m1"),
            test_message("m2"),
            test_message("m3"),
        ];

        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(
            app.message_cursor,
            Some(2),
            "first Up should park cursor on the newest message"
        );
        assert_eq!(app.scroll_offset, 5, "should NOT scroll on cursor init");

        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.message_cursor, Some(1), "second Up walks back");
        assert_eq!(app.scroll_offset, 5, "still no scrolling mid-list");

        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.message_cursor, Some(0), "reach the oldest message");

        // Now we're at the edge: another Up falls through to a scroll.
        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.message_cursor, Some(0), "cursor stays at the edge");
        assert_eq!(app.scroll_offset, 4, "Up at edge falls through to scroll");
    }

    #[test]
    fn down_arrow_in_empty_composer_with_messages_walks_forward() {
        let mut app = App::default();
        app.scroll_offset = 0;
        app.messages = vec![test_message("m1"), test_message("m2")];
        app.message_cursor = Some(0);

        handle_composer_key(&mut app, key(KeyCode::Down));
        assert_eq!(app.message_cursor, Some(1), "Down walks forward");

        // Down at the newest message falls through to scroll.
        handle_composer_key(&mut app, key(KeyCode::Down));
        assert_eq!(app.message_cursor, Some(1));
        assert_eq!(app.scroll_offset, 1, "Down at edge falls through to scroll");
    }

    fn test_message(id: &str) -> pb::MessageProto {
        pb::MessageProto {
            id: id.into(),
            chat_id: "c1".into(),
            ..Default::default()
        }
    }

    #[test]
    fn up_arrow_with_single_line_text_still_scrolls_messages() {
        // Text that has no embedded newlines is a single logical row, so the
        // cursor is on row 0 and Up should always scroll history.
        let mut app = App::default();
        app.composer.insert_str("hello world");
        app.scroll_offset = 5;

        handle_composer_key(&mut app, key(KeyCode::Up));

        assert_eq!(app.scroll_offset, 4);
        assert_eq!(
            app.composer_text(),
            "hello world",
            "composer text must not be modified"
        );
    }

    #[test]
    fn up_arrow_on_second_row_moves_cursor_within_composer() {
        // Multi-line draft with cursor on row 1: Up should move within the
        // textarea, not scroll history.
        let mut app = App::default();
        app.composer.insert_str("line one");
        app.composer.insert_newline();
        app.composer.insert_str("line two");
        let initial_scroll = app.scroll_offset;
        assert_eq!(app.composer.cursor().0, 1);

        handle_composer_key(&mut app, key(KeyCode::Up));

        assert_eq!(
            app.scroll_offset, initial_scroll,
            "messages pane must not scroll while moving cursor"
        );
        assert_eq!(
            app.composer.cursor().0,
            0,
            "cursor should have moved up to row 0"
        );
    }

    #[test]
    fn down_arrow_on_first_row_of_multiline_moves_cursor_within_composer() {
        let mut app = App::default();
        app.composer.insert_str("line one");
        app.composer.insert_newline();
        app.composer.insert_str("line two");
        // Move cursor back to row 0 by sending Up first.
        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.composer.cursor().0, 0);
        let initial_scroll = app.scroll_offset;

        handle_composer_key(&mut app, key(KeyCode::Down));

        assert_eq!(
            app.scroll_offset, initial_scroll,
            "messages pane must not scroll while cursor moves down within composer"
        );
        assert_eq!(app.composer.cursor().0, 1, "cursor should reach row 1");
    }

    #[test]
    fn down_arrow_on_last_row_of_multiline_scrolls_messages() {
        // After typing a 2-line draft the cursor sits on the last row, so the
        // next Down has no cursor target left and falls through to scrolling.
        let mut app = App::default();
        app.composer.insert_str("line one");
        app.composer.insert_newline();
        app.composer.insert_str("line two");
        assert_eq!(app.composer.cursor().0, 1);
        app.scroll_offset = 5;

        handle_composer_key(&mut app, key(KeyCode::Down));

        assert_eq!(app.scroll_offset, 6, "Down on last row should scroll history");
    }

    #[test]
    fn up_arrow_on_first_row_of_multiline_scrolls_messages() {
        // Symmetric to the Down-on-last-row case.
        let mut app = App::default();
        app.composer.insert_str("line one");
        app.composer.insert_newline();
        app.composer.insert_str("line two");
        // Move cursor up to row 0.
        handle_composer_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.composer.cursor().0, 0);
        app.scroll_offset = 5;

        handle_composer_key(&mut app, key(KeyCode::Up));

        assert_eq!(
            app.scroll_offset, 4,
            "Up on first row of a multi-line draft should scroll history"
        );
    }

    // -- Attachment shortcuts ------------------------------------------------

    fn doc_message(id: &str, file: &str) -> pb::MessageProto {
        pb::MessageProto {
            id: id.into(),
            chat_id: "c1".into(),
            kind: pb::MessageKind::Document as i32,
            file_name: file.into(),
            ..Default::default()
        }
    }

    fn text_message(id: &str, text: &str) -> pb::MessageProto {
        pb::MessageProto {
            id: id.into(),
            chat_id: "c1".into(),
            kind: pb::MessageKind::Text as i32,
            text: text.into(),
            ..Default::default()
        }
    }

    #[test]
    fn s_on_cursored_attachment_emits_download_for_that_message() {
        let mut app = App::default();
        app.messages = vec![text_message("m0", "hi"), doc_message("m1", "spec.pdf")];
        app.message_cursor = Some(1);

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('s')))
            .expect("s should produce a command for an attachment under the cursor");
        match cmd.msg.unwrap() {
            pb::client_message::Msg::DownloadMedia(d) => {
                assert_eq!(d.message_id, "m1", "should target the cursored message");
            }
            other => panic!("expected DownloadMedia, got {other:?}"),
        }
    }

    #[test]
    fn o_on_cursored_attachment_emits_open_for_that_message() {
        let mut app = App::default();
        app.messages = vec![doc_message("m1", "spec.pdf")];
        app.message_cursor = Some(0);

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('o')))
            .expect("o should produce a command");
        match cmd.msg.unwrap() {
            pb::client_message::Msg::OpenMedia(o) => assert_eq!(o.message_id, "m1"),
            other => panic!("expected OpenMedia, got {other:?}"),
        }
    }

    #[test]
    fn s_with_no_cursor_falls_back_to_latest_message() {
        // Convenience path: hit `s` without ever pressing Up. Should target
        // the most recent message in view.
        let mut app = App::default();
        app.messages = vec![doc_message("m1", "old.pdf"), doc_message("m2", "new.pdf")];

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('s'))).expect("cmd");
        match cmd.msg.unwrap() {
            pb::client_message::Msg::DownloadMedia(d) => {
                assert_eq!(d.message_id, "m2", "fallback should be the newest message");
            }
            _ => panic!("wrong variant"),
        }
    }

    #[test]
    fn s_on_text_only_message_shows_info_toast_and_sends_nothing() {
        let mut app = App::default();
        app.messages = vec![text_message("m1", "just text")];
        app.message_cursor = Some(0);

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('s')));
        assert!(cmd.is_none(), "must not send a download command for non-attachment");
        let toast = app.toast.as_ref().expect("should have toasted an explanation");
        assert!(
            toast.text.to_lowercase().contains("no attachment"),
            "toast should explain why nothing happened, got {:?}",
            toast.text
        );
    }

    #[test]
    fn copyable_content_prefers_transcript_over_translation_and_text() {
        let mut app = App::default();
        app.messages = vec![text_message("m1", "original text")];
        app.translations.insert("m1".into(), "translated text".into());
        app.transcriptions.insert("m1".into(), "the transcript".into());

        let msg = app.messages[0].clone();
        assert_eq!(copyable_content(&app, &msg).as_deref(), Some("the transcript"));

        // Drop the transcript: translation should win next.
        app.transcriptions.clear();
        assert_eq!(copyable_content(&app, &msg).as_deref(), Some("translated text"));

        // Drop the translation too: fall back to the message body.
        app.translations.clear();
        assert_eq!(copyable_content(&app, &msg).as_deref(), Some("original text"));
    }

    #[test]
    fn copyable_content_is_none_when_nothing_worth_copying() {
        let mut app = App::default();
        // A bare media message with no caption, transcript, or translation.
        app.messages = vec![doc_message("m1", "photo.jpg")];
        app.messages[0].text = String::new();
        let msg = app.messages[0].clone();
        assert!(copyable_content(&app, &msg).is_none());
    }

    #[test]
    fn c_on_message_with_no_content_shows_info_toast_and_sends_nothing() {
        let mut app = App::default();
        let mut m = doc_message("m1", "photo.jpg");
        m.text = String::new();
        app.messages = vec![m];
        app.message_cursor = Some(0);

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('c')));
        assert!(cmd.is_none(), "copy is a local action, never a server command");
        let toast = app.toast.as_ref().expect("should explain why nothing was copied");
        assert!(
            toast.text.to_lowercase().contains("nothing to copy"),
            "got {:?}",
            toast.text
        );
    }

    #[test]
    fn s_with_text_in_composer_is_treated_as_text_input() {
        // Letter shortcuts must not steal keystrokes once the user starts
        // typing — otherwise a draft like "see attached" would be impossible.
        let mut app = App::default();
        app.messages = vec![doc_message("m1", "f.pdf")];
        app.message_cursor = Some(0);
        app.composer.insert_str("see");

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('s')));
        assert!(cmd.is_none(), "no command - the 's' should be appended to draft");
        assert_eq!(app.composer_text(), "sees", "letter should be inserted");
    }

    #[test]
    fn d_then_y_revokes_the_cursored_message() {
        let mut app = App::default();
        app.messages = vec![doc_message("m1", "wrong.pdf")];
        app.message_cursor = Some(0);

        // First press: arms confirmation, no command yet.
        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('d')));
        assert!(cmd.is_none(), "d alone must not delete");
        assert_eq!(
            app.pending_revoke.as_deref(),
            Some("m1"),
            "should have armed a revoke for the right message"
        );

        // Second press: y confirms.
        let cmd =
            handle_composer_key(&mut app, key(KeyCode::Char('y'))).expect("y should fire");
        match cmd.msg.unwrap() {
            pb::client_message::Msg::Revoke(r) => assert_eq!(r.message_id, "m1"),
            other => panic!("expected Revoke, got {other:?}"),
        }
        assert!(app.pending_revoke.is_none(), "pending revoke must be cleared");
    }

    #[test]
    fn d_then_other_key_cancels_revoke() {
        let mut app = App::default();
        app.messages = vec![doc_message("m1", "wrong.pdf")];
        app.message_cursor = Some(0);

        handle_composer_key(&mut app, key(KeyCode::Char('d')));
        assert!(app.pending_revoke.is_some(), "armed");

        let cmd = handle_composer_key(&mut app, key(KeyCode::Char('n')));
        assert!(cmd.is_none(), "no revoke should fire");
        assert!(app.pending_revoke.is_none(), "armed state must clear");
        assert_eq!(
            app.composer_text(),
            "",
            "the 'n' must NOT be inserted into the composer (it cancelled the revoke)"
        );
    }

    // -- Help modal -----------------------------------------------------------

    #[test]
    fn question_mark_opens_help_when_composer_empty() {
        let mut app = App::default();
        assert!(!app.show_help);

        handle_key(&mut app, key(KeyCode::Char('?')));

        assert!(app.show_help, "? should open the help modal");
    }

    #[test]
    fn f1_opens_help_even_with_text_in_composer() {
        let mut app = App::default();
        app.composer.insert_str("draft message");

        handle_key(&mut app, key(KeyCode::F(1)));

        assert!(app.show_help, "F1 must always open help");
        assert_eq!(
            app.composer_text(),
            "draft message",
            "F1 must not modify composer text"
        );
    }

    #[test]
    fn question_mark_is_text_input_when_composer_nonempty() {
        let mut app = App::default();
        app.composer.insert_str("hi");
        // Focus is composer-side so '?' would otherwise route to the textarea.
        app.focus = FocusPane::Messages;

        handle_key(&mut app, key(KeyCode::Char('?')));

        assert!(!app.show_help, "? must not hijack typing mid-message");
        assert_eq!(
            app.composer_text(),
            "hi?",
            "? should be inserted as a normal character into the draft"
        );
    }

    #[test]
    fn esc_closes_help_modal() {
        let mut app = App::default();
        app.show_help = true;

        handle_key(&mut app, key(KeyCode::Esc));

        assert!(!app.show_help, "Esc should close the help modal");
    }

    #[test]
    fn q_closes_help_modal_without_quitting_app() {
        let mut app = App::default();
        app.show_help = true;
        assert!(app.running);

        handle_key(&mut app, key(KeyCode::Char('q')));

        assert!(!app.show_help, "q should close the help modal");
        assert!(app.running, "plain q must not quit the app");
    }

    #[test]
    fn ctrl_q_quits_even_when_help_open() {
        // Quitting is the one global escape hatch we always want to honor,
        // even when modal UI is showing.
        let mut app = App::default();
        app.show_help = true;

        let key = crossterm::event::KeyEvent::new(KeyCode::Char('q'), KeyModifiers::CONTROL);
        handle_key(&mut app, key);

        assert!(!app.running, "Ctrl+Q must quit even when help is open");
    }

    #[test]
    fn random_key_with_help_open_is_swallowed() {
        let mut app = App::default();
        app.show_help = true;
        let initial_scroll = app.scroll_offset;

        handle_key(&mut app, key(KeyCode::Char('a')));
        handle_key(&mut app, key(KeyCode::Enter));

        assert!(app.show_help, "modal must remain open");
        assert_eq!(app.composer_text(), "", "keys must not reach composer");
        assert_eq!(
            app.scroll_offset, initial_scroll,
            "messages pane must not scroll while modal is open"
        );
    }

    fn make_chat(id: &str, name: &str) -> pb::ChatProto {
        pb::ChatProto {
            id: id.to_string(),
            is_group: false,
            name: name.to_string(),
            unread: 0,
            last_message: 1,
            archived: false,
            pinned: false,
        }
    }

    fn select_chat_intent(msg: &ClientMessage) -> Option<pb::select_chat::Intent> {
        match msg.msg.as_ref()? {
            pb::client_message::Msg::SelectChat(sc) => {
                pb::select_chat::Intent::try_from(sc.intent).ok()
            }
            _ => None,
        }
    }

    fn app_with_two_chats() -> App {
        let mut app = App::default();
        app.chats = vec![make_chat("a@s.whatsapp.net", "Alice"), make_chat("b@s.whatsapp.net", "Bob")];
        app.focus = FocusPane::ChatList;
        app
    }

    #[test]
    fn down_arrow_in_chat_list_sends_probe_intent() {
        // The whole point of the dwell-timer feature: just arrowing past a
        // chat must NOT mark it read. The wire signal for that is the PROBE
        // intent on SelectChat.
        let mut app = app_with_two_chats();

        let msg = handle_key(&mut app, key(KeyCode::Down)).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Probe),
            "Down-arrow navigation must be a PROBE so the backend defers mark-read"
        );
    }

    #[test]
    fn up_arrow_in_chat_list_sends_probe_intent() {
        let mut app = app_with_two_chats();
        app.chat_list_index = 1;

        let msg = handle_key(&mut app, key(KeyCode::Up)).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Probe),
            "Up-arrow navigation must be a PROBE"
        );
    }

    #[test]
    fn ctrl_n_in_chat_list_sends_probe_intent() {
        let mut app = app_with_two_chats();

        let evt = crossterm::event::KeyEvent::new(KeyCode::Char('n'), KeyModifiers::CONTROL);
        let msg = handle_key(&mut app, evt).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Probe),
            "Ctrl+N (vim-style next chat) must be a PROBE - it's keyboard nav"
        );
    }

    #[test]
    fn ctrl_p_in_chat_list_sends_probe_intent() {
        let mut app = app_with_two_chats();
        app.chat_list_index = 1;

        let evt = crossterm::event::KeyEvent::new(KeyCode::Char('p'), KeyModifiers::CONTROL);
        let msg = handle_key(&mut app, evt).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Probe),
            "Ctrl+P must be a PROBE"
        );
    }

    #[test]
    fn enter_in_chat_list_sends_commit_intent() {
        // Enter is the explicit "I want this chat" gesture - it also moves
        // focus into the composer. Backend should mark read immediately.
        let mut app = app_with_two_chats();

        let msg = handle_key(&mut app, key(KeyCode::Enter)).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Commit),
            "Enter on a chat row must be COMMIT so the backend marks read immediately"
        );
        assert_eq!(
            app.focus, FocusPane::Messages,
            "Enter should also move focus to the composer"
        );
    }

    #[test]
    fn typing_a_filter_char_sends_probe_intent() {
        // Typing into the live filter narrows the visible list and reselects;
        // user is searching, not committing.
        let mut app = app_with_two_chats();

        let msg = handle_key(&mut app, key(KeyCode::Char('A'))).expect("expected SelectChat");

        assert_eq!(
            select_chat_intent(&msg),
            Some(pb::select_chat::Intent::Probe),
            "live-filter typing must be PROBE"
        );
    }

    #[test]
    fn arrow_keys_scroll_help_modal_not_messages() {
        // While the modal is open, Down should advance the help scroll
        // offset, not scroll the messages pane behind it.
        let mut app = App::default();
        app.show_help = true;
        let initial_messages_scroll = app.scroll_offset;
        assert_eq!(app.help_scroll, 0);

        handle_key(&mut app, key(KeyCode::Down));
        assert_eq!(app.help_scroll, 1);

        handle_key(&mut app, key(KeyCode::PageDown));
        assert_eq!(app.help_scroll, 11);

        handle_key(&mut app, key(KeyCode::Up));
        assert_eq!(app.help_scroll, 10);

        handle_key(&mut app, key(KeyCode::Home));
        assert_eq!(app.help_scroll, 0);

        assert_eq!(
            app.scroll_offset, initial_messages_scroll,
            "messages-pane scroll offset must remain untouched"
        );
    }
}
