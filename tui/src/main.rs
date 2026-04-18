use std::path::PathBuf;
use std::time::Duration;

use color_eyre::Result;
use crossterm::event::{self, Event, KeyCode, KeyModifiers};
use futures::StreamExt;
use ratatui::DefaultTerminal;

use whatscli_tui::client;
use whatscli_tui::media;
use whatscli_tui::proto::whatscli::{self as pb, ClientMessage};
use whatscli_tui::state::{App, ConnectionState, InputMode};
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

    let result = run_app(&mut terminal, &mut app, client_handle.as_ref(), &mut grpc_rx).await;

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
        Event::Key(key) => handle_key(app, key),
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
                return select_current_chat(app);
            }
            KeyCode::Char('p') => {
                navigate_chat(app, -1);
                return select_current_chat(app);
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
            _ => {}
        }
    }

    match app.input_mode {
        InputMode::Normal => handle_normal_key(app, key),
        InputMode::Command | InputMode::Search => handle_input_key(app, key),
    }
}

fn handle_normal_key(app: &mut App, key: event::KeyEvent) -> Option<ClientMessage> {
    match key.code {
        KeyCode::Tab => {
            app.focus_next();
            None
        }
        KeyCode::Up => {
            if app.focus_is_chat_list() {
                navigate_chat(app, -1);
                select_current_chat(app)
            } else {
                app.scroll_up(1);
                None
            }
        }
        KeyCode::Down => {
            if app.focus_is_chat_list() {
                navigate_chat(app, 1);
                select_current_chat(app)
            } else {
                app.scroll_down(1);
                None
            }
        }
        KeyCode::PageUp => {
            app.scroll_up(10);
            None
        }
        KeyCode::PageDown => {
            app.scroll_down(10);
            None
        }
        KeyCode::Enter => {
            if app.focus_is_chat_list() {
                if app.is_archived_folder_selected() {
                    app.show_archived = !app.show_archived;
                    return None;
                }
                select_current_chat(app)
            } else if !app.input_buffer.is_empty() {
                let text = std::mem::take(&mut app.input_buffer);
                if text.starts_with('/') {
                    return parse_command(app, &text[1..]);
                }
                if let Some(ref chat_id) = app.current_chat {
                    return Some(ClientMessage {
                        msg: Some(pb::client_message::Msg::SendText(pb::SendText {
                            chat_id: chat_id.clone(),
                            text,
                        })),
                    });
                }
                None
            } else {
                None
            }
        }
        KeyCode::Char(c) => {
            if c == '/' {
                app.input_mode = InputMode::Command;
                app.input_buffer.clear();
            } else if app.focus_is_chat_list() {
                app.chat_filter.push(c);
                app.chat_list_index = 0;
                return select_current_chat(app);
            } else {
                app.input_buffer.push(c);
            }
            None
        }
        KeyCode::Backspace => {
            if app.focus_is_chat_list() && !app.chat_filter.is_empty() {
                app.chat_filter.pop();
                app.chat_list_index = 0;
                return select_current_chat(app);
            }
            app.input_buffer.pop();
            None
        }
        KeyCode::Esc => {
            if app.input_mode != InputMode::Normal {
                app.input_mode = InputMode::Normal;
                app.input_buffer.clear();
            } else if app.focus_is_chat_list() && !app.chat_filter.is_empty() {
                app.chat_filter.clear();
                app.chat_list_index = 0;
                return select_current_chat(app);
            }
            None
        }
        _ => None,
    }
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

fn select_current_chat(app: &mut App) -> Option<ClientMessage> {
    let chat_id = app.chat_id_at_index(app.chat_list_index)?;
    app.current_chat = Some(chat_id.clone());
    app.messages.clear();
    app.scroll_offset = usize::MAX;
    app.follow_tail = true;
    app.image_cache.clear();
    Some(ClientMessage {
        msg: Some(pb::client_message::Msg::SelectChat(pb::SelectChat {
            chat_id,
        })),
    })
}
