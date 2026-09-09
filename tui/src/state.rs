use std::collections::{HashMap, VecDeque};
use std::time::Duration;

use ratatui::style::{Color, Style};
use ratatui::widgets::{Block, Borders};
use tui_textarea::{TextArea, WrapMode};

use crate::proto::whatscli::{self as pb, ChatProto, ClientMessage, MessageProto, ServerEvent};

/// Maximum visible rows the composer can grow to before it starts scrolling
/// internally. Matches Slack-style behavior (1 row min, 5 rows max).
pub const COMPOSER_MIN_ROWS: u16 = 3; // 1 line of text + top/bottom border = 3
pub const COMPOSER_MAX_ROWS: u16 = 7; // 5 lines of text + top/bottom border = 7

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum FocusPane {
    ChatList,
    Messages,
}

#[derive(Debug, Clone, PartialEq)]
pub enum ConnectionState {
    Connecting,
    Connected,
    Disconnected,
    Reconnecting,
}

#[derive(Debug, Clone, PartialEq)]
pub enum InputMode {
    Normal,
    Command,
    Search,
}

#[derive(Debug, Clone, PartialEq)]
pub enum ChatRow {
    ArchivedHeader,
    Chat(String),
}

/// Severity of a transient toast — drives the colour we render it in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ToastLevel {
    Info,
    Success,
    Error,
}

/// A short-lived message overlaid on the status bar (e.g. "Saved → /path",
/// "Deleted", "No attachment on this message"). Fully owned by the TUI;
/// the backend has no visibility into toasts.
#[derive(Debug, Clone)]
pub struct Toast {
    pub text: String,
    pub level: ToastLevel,
    /// Wall-clock time at which this toast should disappear. Polled each
    /// frame by `App::tick_toast`.
    pub expires_at: std::time::Instant,
}

/// What `chat_list_index` is "really" pointing at. Captured before any
/// re-sort of the chat list so the cursor can be re-pinned to the same
/// logical row (chat / archived header) afterwards instead of staying on
/// the same numeric row index — that's the bug where a chat that gets
/// auto-marked-read and falls down the list takes the highlight with it
/// because the cursor stays at row N but row N is now a different chat.
#[derive(Debug, Clone, PartialEq)]
enum CursorAnchor {
    /// Cursor was on a chat row; remember its id so we can find it again
    /// after the rebuild.
    Chat(String),
    /// Cursor was parked on the "▶/▼ Archived" header row.
    ArchivedHeader,
    /// Cursor pointed past the end of the list (e.g. empty list, or the
    /// list shrank). Caller will clamp.
    None,
}

pub struct App {
    pub running: bool,

    pub chats: Vec<ChatProto>,
    pub archived_chats: Vec<ChatProto>,
    pub current_chat: Option<String>,
    pub messages: Vec<MessageProto>,

    pub translations: HashMap<String, String>,
    pub transcriptions: HashMap<String, String>,

    pub connection_state: ConnectionState,
    pub reconnect_delay: Duration,
    pub pending_commands: VecDeque<ClientMessage>,

    pub battery_charge: i32,
    pub battery_loading: bool,
    pub wa_connected: bool,
    pub last_seen: String,

    pub scroll_offset: usize,
    pub follow_tail: bool,
    pub chat_list_index: usize,

    /// Cursor into `messages` when the messages pane has focus. `None` means
    /// "no message selected" (default until the user starts navigating with
    /// Up/Down). When `Some(i)`, the i-th message in the *current* chat is
    /// highlighted and is the implicit target of attachment shortcuts
    /// (`s` save, `o` open, `d` delete).
    ///
    /// Reset whenever the chat changes; clamped on every messages-list rebuild
    /// so backlog loads / new messages can't leave the cursor pointing past
    /// the end.
    pub message_cursor: Option<usize>,

    /// In-session map of message id → final on-disk path of the saved file.
    /// Populated when a `FileReady` event arrives for an attachment the user
    /// downloaded with `s`/`o`. Drives the dim "→ saved to …" annotation
    /// rendered under the message in the chat view.
    pub saved_files: HashMap<String, String>,

    /// Transient one-line status to show in the top bar instead of the
    /// connection/chat indicator. Cleared once `toast_expires_at` is in the
    /// past. We don't run a timer thread; the main event loop's tick polls
    /// this every frame and clears stale toasts when it next renders.
    pub toast: Option<Toast>,

    /// Pending revoke confirmation: when set, the next keystroke is
    /// interpreted as a confirmation of the destructive `d` (delete) command
    /// for the given message id. Any non-`y`/`Y` keystroke cancels.
    pub pending_revoke: Option<String>,

    /// Multi-line message composer for the Normal input mode. Owns its own
    /// cursor and supports readline-style editing via `tui-textarea`.
    pub composer: TextArea<'static>,
    /// Single-line buffer used by the modal Command (`/foo`) and Search modes.
    pub input_buffer: String,
    pub input_mode: InputMode,

    pub show_archived: bool,
    /// When true, the chat list is filtered to chats with unread > 0. The
    /// Archived folder header is hidden in this mode but unread archived chats
    /// are still listed inline (so a buried high-priority group is reachable).
    pub show_unread_only: bool,
    /// When true, a modal help popup overlays the rest of the UI. While set,
    /// keystrokes other than Esc/?/q/F1 are swallowed so the popup behaves
    /// truly modally and can't accidentally send a message or pick a chat.
    pub show_help: bool,
    /// Vertical scroll offset for the help modal, measured in lines.
    pub help_scroll: u16,
    pub focus: FocusPane,
    pub search_query: String,
    pub chat_filter: String,

    pub model_progress: Option<(String, f64)>,
    pub qr_code: Option<String>,
    pub login_in_progress: bool,

    pub info_messages: Vec<String>,
    pub error_messages: Vec<String>,

    pub image_cache: crate::media::ImageCache,
}

impl Default for App {
    fn default() -> Self {
        Self {
            running: true,
            chats: Vec::new(),
            archived_chats: Vec::new(),
            current_chat: None,
            messages: Vec::new(),
            translations: HashMap::new(),
            transcriptions: HashMap::new(),
            connection_state: ConnectionState::Connecting,
            reconnect_delay: Duration::from_millis(500),
            pending_commands: VecDeque::new(),
            battery_charge: 0,
            battery_loading: false,
            wa_connected: false,
            last_seen: String::new(),
            scroll_offset: 0,
            follow_tail: true,
            chat_list_index: 0,
            message_cursor: None,
            saved_files: HashMap::new(),
            toast: None,
            pending_revoke: None,
            composer: build_composer(),
            input_buffer: String::new(),
            input_mode: InputMode::Normal,
            show_archived: false,
            show_unread_only: false,
            show_help: false,
            help_scroll: 0,
            focus: FocusPane::ChatList,
            search_query: String::new(),
            chat_filter: String::new(),
            model_progress: None,
            qr_code: None,
            login_in_progress: false,
            info_messages: Vec::new(),
            error_messages: Vec::new(),
            image_cache: crate::media::ImageCache::new(),
        }
    }
}

/// Sort chats so that unread chats float to the top, with each group (unread
/// vs. read) ordered by recency (most recent first). Stable sort, so equal
/// keys preserve incoming order.
fn sort_unread_first(chats: &mut [&ChatProto]) {
    chats.sort_by(|a, b| {
        let a_unread = a.unread > 0;
        let b_unread = b.unread > 0;
        // Unread-first: false < true, so we invert with !.
        match b_unread.cmp(&a_unread) {
            std::cmp::Ordering::Equal => b.last_message.cmp(&a.last_message),
            other => other,
        }
    });
}

/// Build a fresh composer with the styling and growth limits we want.
fn build_composer() -> TextArea<'static> {
    let mut ta = TextArea::default();
    ta.set_block(
        Block::default()
            .borders(Borders::ALL)
            .title(" Input ")
            .border_style(Style::default().fg(Color::Blue)),
    );
    ta.set_style(Style::default().fg(Color::White));
    // Don't visually highlight the cursor's line; we only care about the cursor.
    ta.set_cursor_line_style(Style::default());
    ta.set_wrap_mode(WrapMode::WordOrGlyph);
    ta.set_min_rows(COMPOSER_MIN_ROWS);
    ta.set_max_rows(COMPOSER_MAX_ROWS);
    ta
}

impl App {
    pub fn new() -> Self {
        Self::default()
    }

    /// Returns the composer's current contents joined by newlines.
    pub fn composer_text(&self) -> String {
        self.composer.lines().join("\n")
    }

    /// Reset the composer back to an empty state, preserving its styling.
    pub fn composer_reset(&mut self) {
        self.composer = build_composer();
    }

    pub fn focus_next(&mut self) {
        self.focus = match self.focus {
            FocusPane::ChatList => FocusPane::Messages,
            FocusPane::Messages => FocusPane::ChatList,
        };
    }

    pub fn focus_is_chat_list(&self) -> bool {
        self.focus == FocusPane::ChatList
    }

    pub fn is_archived_folder_selected(&self) -> bool {
        matches!(
            self.visible_chat_rows().get(self.chat_list_index),
            Some(ChatRow::ArchivedHeader)
        )
    }

    /// Returns the list of rows currently visible in the chat list, taking
    /// archived-folder collapse state, the active text filter, and the
    /// "show unread only" toggle into account.
    ///
    /// Sorting rule: chats with `unread > 0` always float to the top, sorted
    /// by recency among themselves; remaining chats follow, also by recency.
    /// This makes self-marked-unread (Ctrl+U) and naturally-unread chats
    /// behave as a unified "needs attention" queue.
    ///
    /// Folder scoping: text-filter searches across both main and archived
    /// (so you can find a name regardless of where it lives). The unread-only
    /// view, on the other hand, is scoped to the **currently visible folder**:
    /// when the Archived folder is collapsed, it scans the main inbox only;
    /// when expanded, it scans the archived list only. The Archived header
    /// stays visible in both modes so the user can switch scope.
    pub fn visible_chat_rows(&self) -> Vec<ChatRow> {
        let needle = self.chat_filter.to_lowercase();
        let has_text_filter = !needle.is_empty();

        let matches_filters = |chat: &ChatProto, scope_unread: bool| -> bool {
            if scope_unread && self.show_unread_only && chat.unread <= 0 {
                return false;
            }
            if has_text_filter && !chat.name.to_lowercase().contains(&needle) {
                return false;
            }
            true
        };

        // Text-filter mode (without unread-only) collapses the folder boundary
        // entirely - finding "Gilberth" should work whether the chat lives in
        // main or in Archived. We hide the Archived header in this mode and
        // inline the matching archived chats with the rest.
        if has_text_filter && !self.show_unread_only {
            let mut combined: Vec<&ChatProto> = self
                .chats
                .iter()
                .chain(self.archived_chats.iter())
                .filter(|c| matches_filters(c, false))
                .collect();
            sort_unread_first(&mut combined);
            return combined
                .into_iter()
                .map(|c| ChatRow::Chat(c.id.clone()))
                .collect();
        }

        // From here on, unread-only filtering applies to whichever folder is
        // currently expanded; the Archived header remains in both modes so
        // users can step in/out.
        let mut rows: Vec<ChatRow> = Vec::new();
        let has_archived = !self.archived_chats.is_empty();

        if self.show_archived {
            // "Inside" the Archived folder.
            if has_archived {
                rows.push(ChatRow::ArchivedHeader);
                let mut archived: Vec<&ChatProto> = self
                    .archived_chats
                    .iter()
                    .filter(|c| matches_filters(c, true))
                    .collect();
                sort_unread_first(&mut archived);
                for chat in archived {
                    rows.push(ChatRow::Chat(chat.id.clone()));
                }
            }
            // Main chats are intentionally hidden while the user is "inside"
            // Archived, matching the way phone apps treat folder navigation.
        } else {
            // "Outside" the Archived folder: looking at the main inbox.
            if has_archived {
                rows.push(ChatRow::ArchivedHeader);
            }
            let mut main: Vec<&ChatProto> = self
                .chats
                .iter()
                .filter(|c| matches_filters(c, true))
                .collect();
            sort_unread_first(&mut main);
            for chat in main {
                rows.push(ChatRow::Chat(chat.id.clone()));
            }
        }
        rows
    }

    /// Toggle the "show unread only" view. Clears the chat list cursor so the
    /// selection lands on the first row of whatever is now visible.
    pub fn toggle_unread_only(&mut self) {
        self.show_unread_only = !self.show_unread_only;
        self.chat_list_index = 0;
    }

    /// Show / hide the modal help popup. Resets the scroll on open so the
    /// user always lands at the top of the help.
    pub fn toggle_help(&mut self) {
        self.show_help = !self.show_help;
        if self.show_help {
            self.help_scroll = 0;
        }
    }

    pub fn close_help(&mut self) {
        self.show_help = false;
    }

    pub fn help_scroll_up(&mut self, n: u16) {
        self.help_scroll = self.help_scroll.saturating_sub(n);
    }

    pub fn help_scroll_down(&mut self, n: u16) {
        self.help_scroll = self.help_scroll.saturating_add(n);
    }

    pub fn chat_id_at_index(&self, index: usize) -> Option<String> {
        match self.visible_chat_rows().get(index)? {
            ChatRow::ArchivedHeader => None,
            ChatRow::Chat(id) => Some(id.clone()),
        }
    }

    /// After the chat list has been rebuilt (and possibly re-sorted), put
    /// the cursor back on the same logical row it was on before the
    /// rebuild — so a chat that falls down the list because it just got
    /// marked read still stays highlighted, and the highlight follows it
    /// to its new row.
    ///
    /// Fallback ladder if the anchor disappears (chat got archived,
    /// filter no longer matches, list shrank, …):
    ///
    /// 1. Try to locate the anchored chat / header in the new visible
    ///    rows; if found, snap the cursor there.
    /// 2. Otherwise, snap the cursor to the row the user *currently has
    ///    open* (`current_chat`) so they don't lose context.
    /// 3. Otherwise (anchor was `None` because the user never picked a
    ///    row, e.g. fresh startup), default to the **top** of the list —
    ///    that's where the user expects the cursor to be on first paint.
    /// 4. As a last resort (anchor existed but vanished and no current
    ///    chat), clamp to the last valid row so the cursor stays on
    ///    something reachable.
    fn reanchor_chat_cursor(&mut self, anchor: CursorAnchor) {
        let rows = self.visible_chat_rows();
        if rows.is_empty() {
            self.chat_list_index = 0;
            return;
        }

        let find_chat = |id: &str| -> Option<usize> {
            rows.iter().position(|row| match row {
                ChatRow::Chat(row_id) => row_id == id,
                ChatRow::ArchivedHeader => false,
            })
        };
        let find_header = || -> Option<usize> {
            rows.iter()
                .position(|row| matches!(row, ChatRow::ArchivedHeader))
        };

        let resolved = match &anchor {
            CursorAnchor::Chat(id) => find_chat(id),
            CursorAnchor::ArchivedHeader => find_header(),
            CursorAnchor::None => None,
        }
        .or_else(|| self.current_chat.as_deref().and_then(find_chat));

        let new_idx = match resolved {
            Some(idx) => idx,
            // No anchor at all (fresh startup, or list re-populated while
            // the cursor was on a now-disappeared row in an empty/empty
            // visible state). Anchor::None means we never had a real
            // selection to preserve, so snap to the top — that's the
            // intuitive starting position. The "last row" fallback is
            // reserved for the case where we *did* have a real anchor
            // that vanished, so the cursor stays on something reachable.
            None if matches!(anchor, CursorAnchor::None) => 0,
            None => rows.len() - 1,
        };

        self.chat_list_index = new_idx;
    }

    pub fn scroll_up(&mut self, n: usize) {
        self.scroll_offset = self.scroll_offset.saturating_sub(n);
        self.follow_tail = false;
    }

    pub fn scroll_down(&mut self, n: usize) {
        self.scroll_offset = self.scroll_offset.saturating_add(n);
    }

    /// Default lifetime for transient toasts. Long enough that the user can
    /// glance at a saved-file path before it vanishes; short enough that it
    /// doesn't permanently obscure the chat name in the status bar.
    pub const TOAST_DURATION: Duration = Duration::from_secs(5);

    /// Show a transient one-line message in the top status bar. Replaces
    /// any currently-visible toast — most recent wins.
    pub fn show_toast(&mut self, text: String, level: ToastLevel) {
        self.toast = Some(Toast {
            text,
            level,
            expires_at: std::time::Instant::now() + Self::TOAST_DURATION,
        });
    }

    /// Called from the main loop each frame: drop the toast once its TTL
    /// has expired so the status bar reverts to normal. Returns true if
    /// the toast just expired (so the caller can request a redraw).
    pub fn tick_toast(&mut self) -> bool {
        if let Some(t) = &self.toast {
            if std::time::Instant::now() >= t.expires_at {
                self.toast = None;
                return true;
            }
        }
        false
    }

    /// Move the message cursor by `delta` rows within the current chat,
    /// clamped to `[0, len-1]`. Returns true if the cursor actually moved
    /// (i.e. it was already at the requested edge → false).
    ///
    /// Initialises the cursor to the *last* message on the first downward
    /// move from `None` (matching the user's mental model: history is
    /// pinned to the bottom, so "Up" should select the most recent
    /// message and walk backwards from there).
    pub fn navigate_message(&mut self, delta: i32) -> bool {
        if self.messages.is_empty() {
            return false;
        }
        let last = self.messages.len() - 1;
        let new = match (self.message_cursor, delta) {
            (None, d) if d < 0 => last,           // first Up: jump to newest
            (None, d) if d > 0 => 0,              // first Down: jump to oldest
            (None, _) => return false,
            (Some(_), 0) => return false,
            (Some(idx), d) => {
                let raw = idx as i32 + d;
                raw.clamp(0, last as i32) as usize
            }
        };
        if Some(new) == self.message_cursor {
            return false;
        }
        self.message_cursor = Some(new);
        true
    }

    /// Returns the message under the cursor, if any.
    pub fn cursored_message(&self) -> Option<&MessageProto> {
        self.message_cursor.and_then(|i| self.messages.get(i))
    }

    pub fn filtered_chats(&self) -> Vec<&ChatProto> {
        if self.search_query.is_empty() {
            self.chats.iter().collect()
        } else {
            let q = self.search_query.to_lowercase();
            self.chats
                .iter()
                .filter(|c| c.name.to_lowercase().contains(&q))
                .collect()
        }
    }

    pub fn handle_server_event(&mut self, event: ServerEvent) {
        use pb::server_event::Event;
        let Some(evt) = event.event else { return };

        match evt {
            Event::ChatList(list) => {
                // Capture what the cursor is currently pointing at *before*
                // we rebuild the chat list. The list re-sorts on every
                // ChatList push (mark-read, new message, archive flip…),
                // and a naive index-based cursor would silently snap to a
                // different chat under the user's nose. We re-anchor by id
                // below so the highlight follows the chat the user is
                // actually looking at.
                let cursor_anchor: CursorAnchor = match self
                    .visible_chat_rows()
                    .get(self.chat_list_index)
                {
                    Some(ChatRow::Chat(id)) => CursorAnchor::Chat(id.clone()),
                    Some(ChatRow::ArchivedHeader) => CursorAnchor::ArchivedHeader,
                    None => CursorAnchor::None,
                };

                self.chats.clear();
                self.archived_chats.clear();
                for chat in list.chats {
                    if chat.archived {
                        self.archived_chats.push(chat);
                    } else {
                        self.chats.push(chat);
                    }
                }

                self.reanchor_chat_cursor(cursor_anchor);
            }
            Event::ChatMessages(msgs) => {
                let chat_changed = msgs.chat_id != self.current_chat.clone().unwrap_or_default();
                if !msgs.chat_id.is_empty() {
                    self.current_chat = Some(msgs.chat_id.clone());
                }
                self.messages = msgs.messages;
                self.scroll_offset = usize::MAX;
                self.follow_tail = true;
                if chat_changed {
                    // Don't carry a stale cursor / pending revoke from a
                    // previous chat into a fresh backlog — the indices
                    // mean nothing in the new context.
                    self.message_cursor = None;
                    self.pending_revoke = None;
                } else if let Some(idx) = self.message_cursor {
                    // Same chat, fresh backlog: keep the cursor pinned
                    // to a valid range.
                    self.message_cursor = (!self.messages.is_empty())
                        .then(|| idx.min(self.messages.len() - 1));
                }
            }
            Event::NewMessage(msg) => {
                if let Some(m) = msg.message {
                    let is_current = self
                        .current_chat
                        .as_ref()
                        .is_some_and(|id| id == &m.chat_id);
                    if is_current {
                        self.messages.push(m);
                    }
                }
            }
            Event::NewTranslation(t) => {
                self.translations
                    .insert(t.message_id, t.translated_text);
            }
            Event::NewTranscription(t) => {
                self.transcriptions
                    .insert(t.message_id, t.transcribed_text);
            }
            Event::StatusUpdate(s) => {
                self.battery_charge = s.battery_charge;
                self.battery_loading = s.battery_loading;
                self.wa_connected = s.connected;
                self.last_seen = s.last_seen;
            }
            Event::ErrorEvent(e) => {
                // Mirror the error onto the transient toast bar so the
                // user actually notices a failed download/open right where
                // they were looking, not buried in a hidden info log.
                if !e.text.is_empty() {
                    self.show_toast(e.text.clone(), ToastLevel::Error);
                }
                self.error_messages.push(e.text);
                if self.error_messages.len() > 50 {
                    self.error_messages.remove(0);
                }
            }
            Event::InfoText(t) => {
                self.info_messages.push(t.text);
                if self.info_messages.len() > 50 {
                    self.info_messages.remove(0);
                }
            }
            Event::FileReady(f) => {
                // Backend signals "your save finished". message_id is
                // optional (legacy paths emit just a path); when present,
                // we annotate the message inline in the chat view.
                if !f.message_id.is_empty() {
                    self.saved_files
                        .insert(f.message_id.clone(), f.file_path.clone());
                }
                if !f.file_path.is_empty() {
                    self.show_toast(
                        format!("Saved \u{2192} {}", f.file_path),
                        ToastLevel::Success,
                    );
                }
            }
            Event::OpenFile(o) => {
                // The file was downloaded *and* the OS-default app was
                // told to open it. Surface the path the same way as
                // FileReady so the user knows where it landed.
                if !o.file_path.is_empty() {
                    self.show_toast(
                        format!("Opened \u{2192} {}", o.file_path),
                        ToastLevel::Success,
                    );
                }
            }
            Event::ModelProgress(p) => {
                if p.total_bytes > 0 {
                    self.model_progress = Some((
                        p.model_name,
                        p.downloaded_bytes as f64 / p.total_bytes as f64,
                    ));
                }
                if p.downloaded_bytes >= p.total_bytes {
                    self.model_progress = None;
                }
            }
            // Delivery/read receipts are reflected in the next full message
            // snapshot in the legacy TUI; keep accepting the newer protocol
            // event so regenerated bindings remain exhaustive.
            Event::MessageStatus(_) => {}
            Event::ColorList(_) => {}
        }
    }
}

impl App {
    pub fn with_test_data() -> Self {
        let mut app = App::new();
        app.connection_state = ConnectionState::Connected;
        app.wa_connected = true;
        app.chats = vec![
            ChatProto {
                id: "alice@s.whatsapp.net".into(),
                name: "Alice".into(),
                unread: 3,
                last_message: 1000,
                ..Default::default()
            },
            ChatProto {
                id: "bob@s.whatsapp.net".into(),
                name: "Bob".into(),
                last_message: 2000,
                ..Default::default()
            },
        ];
        app.archived_chats = vec![ChatProto {
            id: "group@g.us".into(),
            name: "Old Group".into(),
            is_group: true,
            archived: true,
            ..Default::default()
        }];
        app.messages = vec![MessageProto {
            id: "msg1".into(),
            chat_id: "alice@s.whatsapp.net".into(),
            contact_name: "Alice".into(),
            contact_short: "Alice".into(),
            text: "Hello!".into(),
            timestamp: 1700000000,
            ..Default::default()
        }];
        app.current_chat = Some("alice@s.whatsapp.net".into());
        app
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_handle_chat_list_event() {
        let mut app = App::new();
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![
                    ChatProto {
                        id: "a@s.whatsapp.net".into(),
                        name: "Alice".into(),
                        archived: false,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "b@g.us".into(),
                        name: "Group".into(),
                        archived: true,
                        ..Default::default()
                    },
                ],
            })),
        };
        app.handle_server_event(event);
        assert_eq!(app.chats.len(), 1);
        assert_eq!(app.archived_chats.len(), 1);
        assert_eq!(app.chats[0].name, "Alice");
        assert_eq!(app.archived_chats[0].name, "Group");
    }

    #[test]
    fn test_handle_new_message() {
        let mut app = App::new();
        app.current_chat = Some("chat1".into());

        let event = ServerEvent {
            event: Some(pb::server_event::Event::NewMessage(pb::NewMessage {
                message: Some(MessageProto {
                    id: "m1".into(),
                    chat_id: "chat1".into(),
                    text: "hello".into(),
                    ..Default::default()
                }),
            })),
        };
        app.handle_server_event(event);
        assert_eq!(app.messages.len(), 1);
        assert_eq!(app.messages[0].text, "hello");
    }

    #[test]
    fn test_handle_new_message_different_chat() {
        let mut app = App::new();
        app.current_chat = Some("chat1".into());

        let event = ServerEvent {
            event: Some(pb::server_event::Event::NewMessage(pb::NewMessage {
                message: Some(MessageProto {
                    id: "m1".into(),
                    chat_id: "other_chat".into(),
                    text: "hello".into(),
                    ..Default::default()
                }),
            })),
        };
        app.handle_server_event(event);
        assert_eq!(app.messages.len(), 0);
    }

    #[test]
    fn test_handle_translation() {
        let mut app = App::new();
        let event = ServerEvent {
            event: Some(pb::server_event::Event::NewTranslation(
                pb::NewTranslation {
                    message_id: "m1".into(),
                    translated_text: "Good morning".into(),
                },
            )),
        };
        app.handle_server_event(event);
        assert_eq!(
            app.translations.get("m1"),
            Some(&"Good morning".to_string())
        );
    }

    #[test]
    fn test_focus_toggle() {
        let mut app = App::new();
        assert_eq!(app.focus, FocusPane::ChatList);
        app.focus_next();
        assert_eq!(app.focus, FocusPane::Messages);
        app.focus_next();
        assert_eq!(app.focus, FocusPane::ChatList);
    }

    #[test]
    fn test_archived_folder_selection() {
        let mut app = App::with_test_data();
        assert!(app.is_archived_folder_selected()); // index 0 with archived chats
        app.chat_list_index = 1;
        assert!(!app.is_archived_folder_selected());
    }

    #[test]
    fn test_scroll() {
        let mut app = App::new();
        app.scroll_offset = 50;
        app.scroll_up(5);
        assert_eq!(app.scroll_offset, 45);
        app.scroll_down(3);
        assert_eq!(app.scroll_offset, 48);
        app.scroll_up(100);
        assert_eq!(app.scroll_offset, 0);
    }

    #[test]
    fn test_follow_tail_disengaged_on_scroll_up() {
        let mut app = App::new();
        assert!(app.follow_tail);
        app.scroll_up(5);
        assert!(!app.follow_tail);
    }

    #[test]
    fn test_chat_filter_filters_by_name() {
        let mut app = App::with_test_data();
        // Without filter: archived header + Alice + Bob = 3 rows (folder collapsed)
        assert_eq!(app.visible_chat_rows().len(), 3);

        app.chat_filter = "ali".into();
        let rows = app.visible_chat_rows();
        assert_eq!(rows.len(), 1);
        match &rows[0] {
            ChatRow::Chat(id) => assert_eq!(id, "alice@s.whatsapp.net"),
            _ => panic!("expected chat row"),
        }
    }

    #[test]
    fn test_chat_filter_includes_archived() {
        let mut app = App::with_test_data();
        app.chat_filter = "old".into();
        let rows = app.visible_chat_rows();
        assert_eq!(rows.len(), 1);
        match &rows[0] {
            ChatRow::Chat(id) => assert_eq!(id, "group@g.us"),
            _ => panic!("expected chat row"),
        }
    }

    #[test]
    fn test_chat_filter_case_insensitive() {
        let mut app = App::with_test_data();
        app.chat_filter = "BOB".into();
        let rows = app.visible_chat_rows();
        assert_eq!(rows.len(), 1);
    }

    #[test]
    fn test_unread_chats_sort_to_top() {
        // Build a scenario where the most-recent chat has zero unread but an
        // older chat is unread. The unread chat must appear above the recent
        // read chat.
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "recent_read@s.whatsapp.net".into(),
                name: "Recent Read".into(),
                unread: 0,
                last_message: 9000,
                ..Default::default()
            },
            ChatProto {
                id: "old_unread@s.whatsapp.net".into(),
                name: "Old Unread".into(),
                unread: 5,
                last_message: 1000,
                ..Default::default()
            },
            ChatProto {
                id: "older_read@s.whatsapp.net".into(),
                name: "Older Read".into(),
                unread: 0,
                last_message: 500,
                ..Default::default()
            },
        ];
        let rows = app.visible_chat_rows();
        let ids: Vec<String> = rows
            .iter()
            .filter_map(|r| match r {
                ChatRow::Chat(id) => Some(id.clone()),
                _ => None,
            })
            .collect();
        assert_eq!(
            ids,
            vec![
                "old_unread@s.whatsapp.net",   // unread first
                "recent_read@s.whatsapp.net",  // then read by recency
                "older_read@s.whatsapp.net",
            ],
            "unread chat should float above more-recent read chats"
        );
    }

    #[test]
    fn test_unread_chats_sorted_by_recency_among_themselves() {
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "u_old@s.whatsapp.net".into(),
                name: "Older Unread".into(),
                unread: 1,
                last_message: 100,
                ..Default::default()
            },
            ChatProto {
                id: "u_new@s.whatsapp.net".into(),
                name: "Newer Unread".into(),
                unread: 1,
                last_message: 200,
                ..Default::default()
            },
        ];
        let rows = app.visible_chat_rows();
        let ids: Vec<String> = rows
            .iter()
            .filter_map(|r| match r {
                ChatRow::Chat(id) => Some(id.clone()),
                _ => None,
            })
            .collect();
        assert_eq!(
            ids,
            vec!["u_new@s.whatsapp.net", "u_old@s.whatsapp.net"],
            "newer unread chat should sort above older unread chat"
        );
    }

    #[test]
    fn test_show_unread_only_filters_to_unread() {
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "read1@s.whatsapp.net".into(),
                name: "Read 1".into(),
                unread: 0,
                last_message: 9000,
                ..Default::default()
            },
            ChatProto {
                id: "unread1@s.whatsapp.net".into(),
                name: "Unread 1".into(),
                unread: 3,
                last_message: 5000,
                ..Default::default()
            },
            ChatProto {
                id: "read2@s.whatsapp.net".into(),
                name: "Read 2".into(),
                unread: 0,
                last_message: 4000,
                ..Default::default()
            },
        ];
        app.toggle_unread_only();
        assert!(app.show_unread_only);
        let rows = app.visible_chat_rows();
        assert_eq!(rows.len(), 1);
        match &rows[0] {
            ChatRow::Chat(id) => assert_eq!(id, "unread1@s.whatsapp.net"),
            _ => panic!("expected chat row"),
        }

        // Toggling back should restore the full list.
        app.toggle_unread_only();
        assert!(!app.show_unread_only);
        assert_eq!(app.visible_chat_rows().len(), 3);
    }

    /// With Archived collapsed (default), the unread-only filter must scope to
    /// the **main** inbox only - it must NOT pull in unread chats from inside
    /// Archived. The Archived header itself stays visible so the user can step
    /// in and see archived unreads.
    #[test]
    fn test_unread_only_excludes_archived_when_folder_collapsed() {
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "main_unread@s.whatsapp.net".into(),
                name: "Main Unread".into(),
                unread: 2,
                last_message: 5000,
                ..Default::default()
            },
            ChatProto {
                id: "main_read@s.whatsapp.net".into(),
                name: "Main Read".into(),
                unread: 0,
                last_message: 4000,
                ..Default::default()
            },
        ];
        app.archived_chats = vec![ChatProto {
            id: "arch_unread@g.us".into(),
            name: "Archived Unread".into(),
            archived: true,
            unread: 30,
            last_message: 2000,
            ..Default::default()
        }];

        assert!(!app.show_archived, "Archived starts collapsed");
        app.toggle_unread_only();
        let rows = app.visible_chat_rows();

        // Header should be the first row, then the one unread main chat. The
        // archived unread chat is NOT in the visible view because we're
        // looking at the main inbox.
        assert_eq!(rows.len(), 2);
        assert!(matches!(rows[0], ChatRow::ArchivedHeader));
        match &rows[1] {
            ChatRow::Chat(id) => assert_eq!(id, "main_unread@s.whatsapp.net"),
            _ => panic!("expected the unread main chat"),
        }
    }

    /// When the user steps "into" the Archived folder (header expanded), the
    /// unread-only filter should scope to archived chats only, and main chats
    /// should disappear from the visible list.
    #[test]
    fn test_unread_only_scopes_to_archived_when_folder_expanded() {
        let mut app = App::new();
        app.chats = vec![ChatProto {
            id: "main_unread@s.whatsapp.net".into(),
            name: "Main Unread".into(),
            unread: 2,
            last_message: 5000,
            ..Default::default()
        }];
        app.archived_chats = vec![
            ChatProto {
                id: "arch_unread@g.us".into(),
                name: "Archived Unread".into(),
                archived: true,
                unread: 30,
                last_message: 3000,
                ..Default::default()
            },
            ChatProto {
                id: "arch_read@g.us".into(),
                name: "Archived Read".into(),
                archived: true,
                unread: 0,
                last_message: 2000,
                ..Default::default()
            },
        ];

        app.show_archived = true;
        app.toggle_unread_only();
        let rows = app.visible_chat_rows();

        // Header + one unread archived chat. Main chats hidden.
        assert_eq!(rows.len(), 2);
        assert!(matches!(rows[0], ChatRow::ArchivedHeader));
        match &rows[1] {
            ChatRow::Chat(id) => assert_eq!(id, "arch_unread@g.us"),
            _ => panic!("expected the unread archived chat"),
        }
    }

    /// Text-filter mode is intentionally cross-folder: typing a name should
    /// find matches in main OR archived. Only the unread-only view is
    /// folder-scoped.
    #[test]
    fn test_text_filter_searches_across_folders() {
        let mut app = App::new();
        app.chats = vec![ChatProto {
            id: "alice@s.whatsapp.net".into(),
            name: "Alice".into(),
            last_message: 5000,
            ..Default::default()
        }];
        app.archived_chats = vec![ChatProto {
            id: "alex@g.us".into(),
            name: "Alex Archived".into(),
            archived: true,
            last_message: 2000,
            ..Default::default()
        }];

        app.chat_filter = "al".into();
        let rows = app.visible_chat_rows();
        let ids: Vec<&str> = rows
            .iter()
            .filter_map(|r| match r {
                ChatRow::Chat(id) => Some(id.as_str()),
                _ => None,
            })
            .collect();
        assert!(ids.contains(&"alice@s.whatsapp.net"));
        assert!(ids.contains(&"alex@g.us"));
    }

    #[test]
    fn test_toggle_unread_only_resets_chat_index() {
        let mut app = App::with_test_data();
        app.chat_list_index = 5;
        app.toggle_unread_only();
        assert_eq!(app.chat_list_index, 0);
    }

    /// The user-visible bug: select an unread chat, dwell, the auto-mark-read
    /// fires, the chat falls down the list, and the highlight gets left
    /// behind on whatever happens to occupy the old row index. Fix: cursor
    /// follows the chat by id, not by row number.
    #[test]
    fn test_chat_list_event_keeps_cursor_on_same_chat_after_resort() {
        let mut app = App::new();
        // Initial sort: alice unread (top), bob read (bottom).
        app.chats = vec![
            ChatProto {
                id: "alice".into(),
                name: "Alice".into(),
                unread: 3,
                last_message: 1000,
                ..Default::default()
            },
            ChatProto {
                id: "bob".into(),
                name: "Bob".into(),
                unread: 0,
                last_message: 2000,
                ..Default::default()
            },
        ];
        // Cursor on Alice (row 0).
        app.chat_list_index = 0;
        let rows_before = app.visible_chat_rows();
        assert!(matches!(&rows_before[0], ChatRow::Chat(id) if id == "alice"));

        // Re-emit the chat list with Alice now read. Sort order flips:
        // Bob (read but more recent) now goes above Alice.
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![
                    ChatProto {
                        id: "alice".into(),
                        name: "Alice".into(),
                        unread: 0,
                        last_message: 1000,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "bob".into(),
                        name: "Bob".into(),
                        unread: 0,
                        last_message: 2000,
                        ..Default::default()
                    },
                ],
            })),
        };
        app.handle_server_event(event);

        let rows_after = app.visible_chat_rows();
        // Both read, sorted by recency: Bob first, Alice second.
        assert!(matches!(&rows_after[0], ChatRow::Chat(id) if id == "bob"));
        assert!(matches!(&rows_after[1], ChatRow::Chat(id) if id == "alice"));
        // Cursor must have followed Alice down to row 1 — NOT stayed on
        // row 0 (where Bob now sits, which the user was not looking at).
        assert_eq!(app.chat_list_index, 1, "cursor should follow Alice to her new row");
    }

    /// Edge case: if the chat the cursor was on has vanished entirely
    /// (e.g. archived, or filtered out), fall back to whatever chat is
    /// currently *open* so the user keeps context, rather than landing
    /// somewhere arbitrary.
    #[test]
    fn test_chat_list_event_falls_back_to_current_chat_when_anchor_gone() {
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "alice".into(),
                name: "Alice".into(),
                last_message: 1000,
                ..Default::default()
            },
            ChatProto {
                id: "bob".into(),
                name: "Bob".into(),
                last_message: 2000,
                ..Default::default()
            },
            ChatProto {
                id: "carol".into(),
                name: "Carol".into(),
                last_message: 3000,
                ..Default::default()
            },
        ];
        // Cursor on the *third* row (Alice — oldest, sorts last by recency).
        // First re-render so visible_chat_rows reflects the recency sort.
        let initial_rows = app.visible_chat_rows();
        let alice_row = initial_rows
            .iter()
            .position(|r| matches!(r, ChatRow::Chat(id) if id == "alice"))
            .unwrap();
        app.chat_list_index = alice_row;
        // User has Carol open in the message pane.
        app.current_chat = Some("carol".into());

        // Server re-emits the list without Alice (got archived).
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![
                    ChatProto {
                        id: "bob".into(),
                        name: "Bob".into(),
                        last_message: 2000,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "carol".into(),
                        name: "Carol".into(),
                        last_message: 3000,
                        ..Default::default()
                    },
                ],
            })),
        };
        app.handle_server_event(event);

        let rows_after = app.visible_chat_rows();
        let carol_row = rows_after
            .iter()
            .position(|r| matches!(r, ChatRow::Chat(id) if id == "carol"))
            .expect("carol should still be visible");
        assert_eq!(
            app.chat_list_index, carol_row,
            "anchor (Alice) is gone, so cursor should land on currently-open chat (Carol)"
        );
    }

    /// Regression: the very first ChatList event after startup must leave
    /// the cursor at the *top* (index 0), not at the bottom. Before the
    /// fix, an empty visible list at startup produced a `CursorAnchor::None`
    /// which then fell through to the "clamp to last row" branch, snapping
    /// the highlight to the very last chat once the list arrived.
    #[test]
    fn test_chat_list_event_at_startup_keeps_cursor_at_top() {
        let mut app = App::new();
        // Fresh app: no chats yet, no current_chat, default cursor at 0.
        assert!(app.chats.is_empty());
        assert!(app.current_chat.is_none());
        assert_eq!(app.chat_list_index, 0);

        // Backend pushes the initial chat list.
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![
                    ChatProto {
                        id: "first".into(),
                        name: "First".into(),
                        last_message: 9999,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "second".into(),
                        name: "Second".into(),
                        last_message: 8000,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "third".into(),
                        name: "Third".into(),
                        last_message: 7000,
                        ..Default::default()
                    },
                ],
            })),
        };
        app.handle_server_event(event);

        assert_eq!(
            app.chat_list_index, 0,
            "fresh startup should leave the cursor on the top-most chat"
        );
    }

    /// Final fallback: anchor gone AND no current_chat set → clamp to last
    /// row instead of overflowing.
    #[test]
    fn test_chat_list_event_clamps_when_anchor_and_current_chat_both_gone() {
        let mut app = App::new();
        app.chats = vec![
            ChatProto {
                id: "alice".into(),
                name: "Alice".into(),
                last_message: 1000,
                ..Default::default()
            },
            ChatProto {
                id: "bob".into(),
                name: "Bob".into(),
                last_message: 2000,
                ..Default::default()
            },
        ];
        app.chat_list_index = 1;
        app.current_chat = None;

        // Shrink to one chat and remove the anchor.
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![ChatProto {
                    id: "carol".into(),
                    name: "Carol".into(),
                    last_message: 3000,
                    ..Default::default()
                }],
            })),
        };
        app.handle_server_event(event);
        assert_eq!(app.chat_list_index, 0, "should clamp into the now-shorter list");
    }

    /// Cursor parked on the Archived header should stay parked on the
    /// Archived header across re-sorts.
    #[test]
    fn test_chat_list_event_keeps_cursor_on_archived_header() {
        let mut app = App::new();
        app.chats = vec![ChatProto {
            id: "alice".into(),
            name: "Alice".into(),
            last_message: 1000,
            ..Default::default()
        }];
        app.archived_chats = vec![ChatProto {
            id: "old@g.us".into(),
            name: "Old".into(),
            archived: true,
            last_message: 500,
            ..Default::default()
        }];
        // Park cursor on the archived header.
        let header_row = app
            .visible_chat_rows()
            .iter()
            .position(|r| matches!(r, ChatRow::ArchivedHeader))
            .unwrap();
        app.chat_list_index = header_row;
        assert!(app.is_archived_folder_selected());

        // Re-emit (e.g. a new message bumps Alice's recency).
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatList(pb::ChatList {
                chats: vec![
                    ChatProto {
                        id: "alice".into(),
                        name: "Alice".into(),
                        last_message: 9999,
                        ..Default::default()
                    },
                    ChatProto {
                        id: "old@g.us".into(),
                        name: "Old".into(),
                        archived: true,
                        last_message: 500,
                        ..Default::default()
                    },
                ],
            })),
        };
        app.handle_server_event(event);

        assert!(
            app.is_archived_folder_selected(),
            "cursor should still be on the archived header after re-sort"
        );
    }

    #[test]
    fn test_follow_tail_engaged_on_chat_messages() {
        let mut app = App::new();
        app.follow_tail = false;
        let event = ServerEvent {
            event: Some(pb::server_event::Event::ChatMessages(pb::ChatMessages {
                chat_id: "chat1".into(),
                messages: vec![],
            })),
        };
        app.handle_server_event(event);
        assert!(app.follow_tail);
        assert_eq!(app.scroll_offset, usize::MAX);
    }
}
