use ratatui::{
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};

use crate::state::{App, ConnectionState, ToastLevel};

pub fn render(f: &mut Frame, area: Rect, app: &App) {
    // A live toast takes over the entire status bar — its text is more
    // important than the chat name in the moment, and we want it impossible
    // to miss. Falls back to the regular layout once the toast expires.
    if let Some(toast) = app.toast.as_ref() {
        render_toast(f, area, toast);
        return;
    }

    let conn_indicator = match app.connection_state {
        ConnectionState::Connected if app.wa_connected => {
            Span::styled("● ", Style::default().fg(Color::Green))
        }
        ConnectionState::Connected => {
            Span::styled("● ", Style::default().fg(Color::Yellow))
        }
        ConnectionState::Connecting | ConnectionState::Reconnecting => {
            Span::styled("◌ ", Style::default().fg(Color::Yellow))
        }
        ConnectionState::Disconnected => {
            Span::styled("○ ", Style::default().fg(Color::Red))
        }
    };

    let mut spans = vec![conn_indicator];

    if let Some(ref chat_id) = app.current_chat {
        let name = app
            .chats
            .iter()
            .chain(app.archived_chats.iter())
            .find(|c| c.id == *chat_id)
            .map(|c| c.name.as_str())
            .unwrap_or(chat_id.as_str());
        spans.push(Span::raw(name.to_string()));
    } else {
        spans.push(Span::raw("WhatsCLI"));
    }

    if app.battery_charge > 0 {
        let batt = if app.battery_loading {
            format!("  ⚡{}%", app.battery_charge)
        } else {
            format!("  🔋{}%", app.battery_charge)
        };
        spans.push(Span::raw(batt));
    }

    if let Some((ref name, pct)) = app.model_progress {
        spans.push(Span::styled(
            format!("  {} {:.0}%", name, pct * 100.0),
            Style::default().fg(Color::Cyan),
        ));
    }

    let line = Line::from(spans);
    let bar = Paragraph::new(line).style(Style::default().bg(Color::DarkGray));
    f.render_widget(bar, area);
}

fn render_toast(f: &mut Frame, area: Rect, toast: &crate::state::Toast) {
    let (fg, marker) = match toast.level {
        ToastLevel::Success => (Color::Green, "✓ "),
        ToastLevel::Error => (Color::Red, "✗ "),
        ToastLevel::Info => (Color::Cyan, "ℹ "),
    };
    let line = Line::from(vec![
        Span::styled(
            marker,
            Style::default().fg(fg).add_modifier(Modifier::BOLD),
        ),
        Span::styled(
            toast.text.clone(),
            Style::default().fg(fg).add_modifier(Modifier::BOLD),
        ),
    ]);
    let bar = Paragraph::new(line).style(Style::default().bg(Color::DarkGray));
    f.render_widget(bar, area);
}
