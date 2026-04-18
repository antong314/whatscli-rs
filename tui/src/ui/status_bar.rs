use ratatui::{
    layout::Rect,
    style::{Color, Style},
    text::{Line, Span},
    widgets::Paragraph,
    Frame,
};

use crate::state::{App, ConnectionState};

pub fn render(f: &mut Frame, area: Rect, app: &App) {
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
