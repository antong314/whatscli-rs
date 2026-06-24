//! Centralized help / keyboard-shortcut reference.
//!
//! This module is the **single source of truth** for the shortcut list. It
//! powers both the inline help shown when no chat is selected (see
//! `message_view`) and the `?` / `F1` modal popup that overlays whatever the
//! user is doing at the moment.
//!
//! When you add a new shortcut, add it here and only here.

use ratatui::{
    layout::{Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Clear, Paragraph},
    Frame,
};

use crate::state::App;

/// Canonical, exhaustive list of shortcuts and slash-commands, grouped by
/// section. Used by both the startup help screen and the modal popup.
pub fn help_lines() -> Vec<Line<'static>> {
    let h = |s: &'static str| Line::from(Span::styled(s, header_style()));
    let blank = || Line::from("");

    let mut lines: Vec<Line<'static>> = Vec::new();
    lines.push(blank());
    lines.push(Line::from(Span::styled(
        "  WhatsCLI - WhatsApp Terminal Client",
        Style::default()
            .fg(Color::Green)
            .add_modifier(Modifier::BOLD),
    )));
    lines.push(blank());

    lines.push(h("  Showing this help"));
    lines.push(blank());
    lines.push(Line::from("    ? / F1       Open this help (?, when composer empty)"));
    lines.push(Line::from("    Esc / ? / q  Close this help"));
    lines.push(blank());

    lines.push(h("  Navigation"));
    lines.push(blank());
    lines.push(Line::from("    Tab          Switch focus between chat list and messages"));
    lines.push(Line::from("    Ctrl+N / P   Next / previous chat"));
    lines.push(Line::from("    Up / Down    In chat list: navigate chats"));
    lines.push(Line::from("                 In composer (single-line): scroll messages"));
    lines.push(Line::from("                 In composer (multi-line): move cursor; scroll at edges"));
    lines.push(Line::from("    PgUp / PgDn  Scroll messages by 10"));
    lines.push(Line::from("    Enter        In chat list: open chat (focus moves to composer)"));
    lines.push(blank());

    lines.push(h("  Search & filtering"));
    lines.push(blank());
    lines.push(Line::from("    (type)       In chat list: live-filter chats by name"));
    lines.push(Line::from("    Esc          Clear chat-list filter"));
        lines.push(Line::from("    Ctrl+I       Toggle 'unread only' (scoped to current folder)"));
    lines.push(Line::from("    Ctrl+F       Open command-bar search"));
    lines.push(blank());

    lines.push(h("  Composing"));
    lines.push(blank());
    lines.push(Line::from("    Enter        Send message"));
    lines.push(Line::from("    Shift+Enter  Insert newline (multi-line message)"));
    lines.push(Line::from("    Left/Right   Move cursor"));
    lines.push(Line::from("    Home / End   Jump to start / end of line"));
    lines.push(Line::from("    Ctrl+A / E   Start / end of line"));
    lines.push(Line::from("    Ctrl+W       Delete previous word"));
    lines.push(Line::from("    Backspace    Delete previous character"));
    lines.push(blank());

    lines.push(h("  Reading"));
    lines.push(blank());
    lines.push(Line::from("    Ctrl+B       Load older messages (backlog)"));
    lines.push(Line::from("    Ctrl+U       Mark current chat as unread"));
    lines.push(Line::from(
        "    (Chats are marked read after ~3s of dwell, or instantly on Enter.)",
    ));
    lines.push(blank());

    lines.push(h("  Message cursor  (composer empty)"));
    lines.push(blank());
    lines.push(Line::from(
        "    Up / Down    Move message cursor (scrolls history at edges)",
    ));
    lines.push(Line::from(
        "    s            Save cursored attachment to ~/Downloads",
    ));
    lines.push(Line::from(
        "    o            Open cursored attachment (download + system app)",
    ));
    lines.push(Line::from(
        "    d  then  y   Delete (revoke) cursored message  [confirm with y]",
    ));
    lines.push(Line::from(
        "    t            Force-translate cursored message (overrides auto-skip)",
    ));
    lines.push(blank());

    lines.push(h("  App"));
    lines.push(blank());
    lines.push(Line::from("    /            Start a slash-command"));
    lines.push(Line::from("    Esc          Cancel current mode / clear filter"));
    lines.push(Line::from("    Ctrl+Q       Quit"));
    lines.push(blank());

    lines.push(h("  Slash-commands (type with leading /)"));
    lines.push(blank());
    lines.push(Line::from("    /login       Connect to WhatsApp"));
    lines.push(Line::from("    /disconnect  Disconnect"));
    lines.push(Line::from("    /logout      Log out completely"));
    lines.push(Line::from("    /backlog     Load older messages"));
    lines.push(Line::from("    /read        Mark chat as read"));
    lines.push(Line::from("    /unread      Mark chat as unread"));
    lines.push(Line::from("    /download    Download last media"));
    lines.push(Line::from("    /open        Open last media"));
    lines.push(Line::from("    /upload      Upload file to chat"));
    lines.push(Line::from("    /info        Message info"));
    lines.push(Line::from("    /url         Open URL from message"));
    lines.push(Line::from("    /revoke      Delete message"));
    lines.push(Line::from("    /leave       Leave group"));
    lines.push(Line::from("    /subject     Set group subject"));

    lines
}

fn header_style() -> Style {
    Style::default()
        .fg(Color::Cyan)
        .add_modifier(Modifier::BOLD)
}

/// Render the help modal centered over `area`. The popup sizes itself to the
/// content (capped at 90% of the available space) and is drawn on top of
/// whatever was already rendered in `area`. When the content is taller than
/// the popup, vertical scrolling is enabled and a hint is added to the title.
///
/// Returns the maximum scroll offset (in lines) so the caller can clamp
/// `app.help_scroll` and avoid scrolling past the end.
pub fn render_modal(f: &mut Frame, area: Rect, app: &App) -> u16 {
    let lines = help_lines();
    let content_lines = lines.len() as u16;
    let desired_height = content_lines + 2; // +2 for the borders.
    let content_width: u16 = 78;

    let popup_area = centered_rect(content_width, desired_height, area);

    let visible_content_rows = popup_area.height.saturating_sub(2);
    let max_scroll = content_lines.saturating_sub(visible_content_rows);
    let scroll = app.help_scroll.min(max_scroll);

    let title = if max_scroll > 0 {
        " Help  (\u{2191}/\u{2193} scroll  -  Esc / ? / q to close) "
    } else {
        " Help  (Esc / ? / q to close) "
    };

    f.render_widget(Clear, popup_area);

    let block = Block::default()
        .borders(Borders::ALL)
        .title(title)
        .border_style(Style::default().fg(Color::Cyan));

    let para = Paragraph::new(lines).block(block).scroll((scroll, 0));
    f.render_widget(para, popup_area);

    max_scroll
}

/// Compute a `width`x`height` rect centered inside `area`, clamped so it can
/// never exceed 90% of the container in either dimension.
fn centered_rect(width: u16, height: u16, area: Rect) -> Rect {
    let max_w = (area.width * 9 / 10).max(10);
    let max_h = (area.height * 9 / 10).max(5);
    let w = width.min(max_w);
    let h = height.min(max_h);

    let vertical = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length((area.height.saturating_sub(h)) / 2),
            Constraint::Length(h),
            Constraint::Min(0),
        ])
        .split(area);

    let horizontal = Layout::default()
        .direction(Direction::Horizontal)
        .constraints([
            Constraint::Length((area.width.saturating_sub(w)) / 2),
            Constraint::Length(w),
            Constraint::Min(0),
        ])
        .split(vertical[1]);

    horizontal[1]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn help_lines_includes_every_shortcut_we_promise() {
        // If we change the help text, leave these phrases in. They're the
        // user-visible promise: every shortcut listed here must exist in the
        // app's actual key handling.
        let rendered: String = help_lines()
            .iter()
            .flat_map(|line| line.spans.iter().map(|s| s.content.as_ref()))
            .collect::<Vec<_>>()
            .join(" ");

        for needle in [
            "Tab",
            "Ctrl+N",
            "Ctrl+B",
            "Ctrl+U",
            "Ctrl+I",
            "Ctrl+F",
            "Ctrl+Q",
            "Up / Down",
            "PgUp / PgDn",
            "Shift+Enter",
            "Esc",
            "?",
            "F1",
        ] {
            assert!(
                rendered.contains(needle),
                "help text missing shortcut '{}'\n---\n{}",
                needle,
                rendered
            );
        }
    }

    #[test]
    fn centered_rect_centers() {
        let area = Rect::new(0, 0, 100, 40);
        let r = centered_rect(40, 20, area);
        assert_eq!(r.width, 40);
        assert_eq!(r.height, 20);
        assert_eq!(r.x, 30, "should be horizontally centered");
        assert_eq!(r.y, 10, "should be vertically centered");
    }

    #[test]
    fn centered_rect_clamps_to_90_percent() {
        let area = Rect::new(0, 0, 50, 20);
        // Ask for something larger than the container.
        let r = centered_rect(200, 200, area);
        assert!(r.width <= 50 * 9 / 10, "width should be clamped to <=90%");
        assert!(r.height <= 20 * 9 / 10, "height should be clamped to <=90%");
    }
}
