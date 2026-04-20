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

    /// Multi-line message composer for the Normal input mode. Owns its own
    /// cursor and supports readline-style editing via `tui-textarea`.
    pub composer: TextArea<'static>,
    /// Single-line buffer used by the modal Command (`/foo`) and Search modes.
    pub input_buffer: String,
    pub input_mode: InputMode,

    pub show_archived: bool,
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
            composer: build_composer(),
            input_buffer: String::new(),
            input_mode: InputMode::Normal,
            show_archived: false,
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
    /// archived-folder collapse state and the active filter into account.
    pub fn visible_chat_rows(&self) -> Vec<ChatRow> {
        let mut rows: Vec<ChatRow> = Vec::new();

        if self.chat_filter.is_empty() {
            if !self.archived_chats.is_empty() {
                rows.push(ChatRow::ArchivedHeader);
                if self.show_archived {
                    for chat in &self.archived_chats {
                        rows.push(ChatRow::Chat(chat.id.clone()));
                    }
                }
            }
            for chat in &self.chats {
                rows.push(ChatRow::Chat(chat.id.clone()));
            }
        } else {
            let needle = self.chat_filter.to_lowercase();
            for chat in self.archived_chats.iter().chain(self.chats.iter()) {
                if chat.name.to_lowercase().contains(&needle) {
                    rows.push(ChatRow::Chat(chat.id.clone()));
                }
            }
        }

        rows
    }

    pub fn chat_id_at_index(&self, index: usize) -> Option<String> {
        match self.visible_chat_rows().get(index)? {
            ChatRow::ArchivedHeader => None,
            ChatRow::Chat(id) => Some(id.clone()),
        }
    }

    pub fn scroll_up(&mut self, n: usize) {
        self.scroll_offset = self.scroll_offset.saturating_sub(n);
        self.follow_tail = false;
    }

    pub fn scroll_down(&mut self, n: usize) {
        self.scroll_offset = self.scroll_offset.saturating_add(n);
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
                self.chats.clear();
                self.archived_chats.clear();
                for chat in list.chats {
                    if chat.archived {
                        self.archived_chats.push(chat);
                    } else {
                        self.chats.push(chat);
                    }
                }
            }
            Event::ChatMessages(msgs) => {
                if !msgs.chat_id.is_empty() {
                    self.current_chat = Some(msgs.chat_id.clone());
                }
                self.messages = msgs.messages;
                self.scroll_offset = usize::MAX;
                self.follow_tail = true;
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
            Event::FileReady(_) | Event::OpenFile(_) => {}
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
