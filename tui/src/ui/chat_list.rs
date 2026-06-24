use ratatui::{
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, List, ListItem, ListState},
    Frame,
};

use crate::state::{App, ChatRow, FocusPane};

pub fn render(f: &mut Frame, area: Rect, app: &App) {
    let border_color = if app.focus == FocusPane::ChatList {
        Color::White
    } else {
        Color::DarkGray
    };

    let rows = app.visible_chat_rows();
    let chat_lookup: std::collections::HashMap<&str, &crate::proto::whatscli::ChatProto> = app
        .archived_chats
        .iter()
        .chain(app.chats.iter())
        .map(|c| (c.id.as_str(), c))
        .collect();

    let items: Vec<ListItem> = rows
        .iter()
        .map(|row| match row {
            ChatRow::ArchivedHeader => archived_header_item(app),
            ChatRow::Chat(id) => chat_lookup
                .get(id.as_str())
                .map(|c| chat_item(c, app))
                .unwrap_or_else(|| ListItem::new(Line::from(""))),
        })
        .collect();

    let mut state = ListState::default();
    if !rows.is_empty() {
        state.select(Some(app.chat_list_index.min(rows.len() - 1)));
    }

    let title = match (app.show_unread_only, app.chat_filter.is_empty()) {
        (true, true) => " Chats (unread) ".to_string(),
        (true, false) => format!(" Chats (unread): {} ", app.chat_filter),
        (false, true) => " Chats ".to_string(),
        (false, false) => format!(" Chats: {} ", app.chat_filter),
    };

    let list = List::new(items)
        .block(
            Block::default()
                .borders(Borders::ALL)
                .title(title)
                .border_style(Style::default().fg(border_color)),
        )
        .highlight_style(
            Style::default()
                .bg(Color::DarkGray)
                .add_modifier(Modifier::BOLD),
        );

    f.render_stateful_widget(list, area, &mut state);
}

fn archived_header_item(app: &App) -> ListItem<'static> {
    let total_unread: i32 = app.archived_chats.iter().map(|c| c.unread).sum();
    let label = if app.show_archived {
        format!("▼ Archived ({})", app.archived_chats.len())
    } else {
        format!("▶ Archived ({})", app.archived_chats.len())
    };
    let mut spans = vec![Span::styled(
        label,
        Style::default()
            .fg(Color::Yellow)
            .add_modifier(Modifier::BOLD),
    )];
    if total_unread > 0 {
        spans.push(Span::raw(" "));
        spans.push(Span::styled(
            format!("[{}]", total_unread),
            Style::default()
                .fg(Color::Yellow)
                .add_modifier(Modifier::BOLD),
        ));
    }
    ListItem::new(Line::from(spans))
}

fn chat_item(chat: &crate::proto::whatscli::ChatProto, app: &App) -> ListItem<'static> {
    let color = if chat.is_group {
        Color::Blue
    } else {
        Color::Green
    };

    let is_current = app
        .current_chat
        .as_ref()
        .is_some_and(|id| id == &chat.id);

    let style = if is_current {
        Style::default()
            .fg(color)
            .add_modifier(Modifier::BOLD | Modifier::UNDERLINED)
    } else {
        Style::default().fg(color)
    };

    let mut spans = vec![Span::styled(chat.name.clone(), style)];

    if chat.unread > 0 {
        spans.push(Span::raw(" "));
        spans.push(Span::styled(
            format!("[{}]", chat.unread),
            Style::default()
                .fg(Color::Yellow)
                .add_modifier(Modifier::BOLD),
        ));
    }

    ListItem::new(Line::from(spans))
}
