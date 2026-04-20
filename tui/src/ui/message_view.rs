use ratatui::{
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Paragraph},
    Frame,
};
use ratatui_image::StatefulImage;

use crate::proto::whatscli::MessageKind;
use crate::state::{App, FocusPane};

const IMAGE_MAX_HEIGHT: u16 = 20;
const IMAGE_MIN_HEIGHT: u16 = 3;

/// Compute the height in terminal cells needed to display an image with the
/// given pixel dimensions, when rendered at `cell_width` columns wide.
/// Falls back to a reasonable default if font size is unknown.
fn image_cell_height(
    image_dims: Option<(u32, u32)>,
    cell_width: u16,
    font_size: Option<(u16, u16)>,
) -> u16 {
    let Some((img_w, img_h)) = image_dims else {
        return IMAGE_MAX_HEIGHT;
    };
    if img_w == 0 || cell_width == 0 {
        return IMAGE_MAX_HEIGHT;
    }
    let (cell_px_w, cell_px_h) = font_size.unwrap_or((7, 14));
    let cell_px_w = cell_px_w.max(1) as f32;
    let cell_px_h = cell_px_h.max(1) as f32;

    let pixels_wide = cell_width as f32 * cell_px_w;
    let scale = pixels_wide / img_w as f32;
    let pixels_tall = img_h as f32 * scale;
    let cells_tall = (pixels_tall / cell_px_h).ceil() as u16;
    cells_tall.clamp(IMAGE_MIN_HEIGHT, IMAGE_MAX_HEIGHT)
}

pub fn render(f: &mut Frame, area: Rect, app: &mut App) {
    let border_color = if app.focus == FocusPane::Messages {
        Color::White
    } else {
        Color::DarkGray
    };

    if app.current_chat.is_none() {
        let help = help_text();
        let para = Paragraph::new(help)
            .block(
                Block::default()
                    .borders(Borders::ALL)
                    .title(" WhatsCLI ")
                    .border_style(Style::default().fg(border_color)),
            );
        f.render_widget(para, area);
        return;
    }

    let inner_width = area.width.saturating_sub(2) as usize;
    let img_width = ((area.width.saturating_sub(2)) * 2 / 3).min(60);
    let font_size = app.image_cache.font_size();
    let mut lines: Vec<Line> = Vec::new();
    let mut image_positions: Vec<(String, usize, u16)> = Vec::new();

    for msg in &app.messages {
        let sender_color = if msg.from_me {
            Color::Blue
        } else {
            Color::Green
        };
        let sender = if msg.from_me {
            "Me".to_string()
        } else if !msg.contact_short.is_empty() {
            msg.contact_short.clone()
        } else {
            msg.contact_name.clone()
        };

        let kind = MessageKind::try_from(msg.kind).unwrap_or(MessageKind::Text);
        let is_image = kind == MessageKind::Image;

        let image_loaded = is_image && app.image_cache.contains(&msg.id);
        let image_loading = is_image && app.image_cache.is_loading(&msg.id);

        let text = match kind {
            MessageKind::Image => {
                if image_loaded {
                    String::new()
                } else if image_loading {
                    "[Loading image...]".to_string()
                } else {
                    "[IMAGE]".to_string()
                }
            }
            MessageKind::Video => "[VIDEO]".to_string(),
            MessageKind::Audio => format!("[AUDIO] {}", msg.text),
            MessageKind::Document => format!("[DOC: {}]", msg.file_name),
            _ => msg.text.clone(),
        };

        let prefix = format!("{}: ", sender);
        let prefix_len = prefix.len();

        if text.is_empty() && image_loaded {
            lines.push(Line::from(Span::styled(
                prefix,
                Style::default()
                    .fg(sender_color)
                    .add_modifier(Modifier::BOLD),
            )));
        } else {
            wrap_message(&mut lines, &prefix, &text, sender_color, inner_width, prefix_len);
        }

        if image_loaded {
            let dims = app.image_cache.dimensions(&msg.id);
            let h = image_cell_height(dims, img_width, font_size);
            image_positions.push((msg.id.clone(), lines.len(), h));
            for _ in 0..h {
                lines.push(Line::from(""));
            }
        }

        let dim_style = Style::default()
            .fg(Color::White)
            .add_modifier(Modifier::DIM);

        if let Some(transcription) = app.transcriptions.get(&msg.id) {
            let label = "[Transcription]: ";
            wrap_styled(&mut lines, label, transcription, dim_style, inner_width);
        }

        if let Some(translation) = app.translations.get(&msg.id) {
            let label = if msg.from_me {
                "Me(EN): ".to_string()
            } else if !msg.contact_short.is_empty() {
                format!("{}(EN): ", msg.contact_short)
            } else {
                format!("{}(EN): ", msg.contact_name)
            };
            wrap_styled(&mut lines, &label, translation, dim_style, inner_width);
        }
    }

    let total_lines = lines.len() as u16;
    let visible = area.height.saturating_sub(2);
    let max_scroll = total_lines.saturating_sub(visible) as usize;
    if app.follow_tail {
        app.scroll_offset = max_scroll;
    } else {
        app.scroll_offset = app.scroll_offset.min(max_scroll);
        // Re-engage tail-following when the user has scrolled all the way down.
        if app.scroll_offset >= max_scroll {
            app.follow_tail = true;
        }
    }
    let scroll = app.scroll_offset as u16;

    let chat_name = app
        .chats
        .iter()
        .chain(app.archived_chats.iter())
        .find(|c| app.current_chat.as_ref().is_some_and(|id| id == &c.id))
        .map(|c| c.name.as_str())
        .unwrap_or("");

    let title = format!(" {} ", chat_name);

    let para = Paragraph::new(lines)
        .block(
            Block::default()
                .borders(Borders::ALL)
                .title(title)
                .border_style(Style::default().fg(border_color)),
        )
        .scroll((scroll, 0));

    f.render_widget(para, area);

    let inner = Rect::new(area.x + 1, area.y + 1, area.width - 2, area.height - 2);
    for (msg_id, line_idx, img_height) in image_positions {
        let line_in_view = line_idx as i32 - scroll as i32;

        // Only render the image when it fits entirely within the visible area.
        // This avoids costly re-encoding by ratatui-image when the image area
        // changes size as it scrolls past the viewport edges, and it keeps
        // the image at a constant size at all times.
        if line_in_view < 0 || line_in_view + img_height as i32 > inner.height as i32 {
            continue;
        }

        let y = inner.y + line_in_view as u16;
        let img_area = Rect::new(inner.x + 1, y, img_width, img_height);

        if let Some(protocol) = app.image_cache.get_mut(&msg_id) {
            let widget = StatefulImage::default();
            f.render_stateful_widget(widget, img_area, protocol);
        }
    }
}

fn wrap_message<'a>(
    lines: &mut Vec<Line<'a>>,
    prefix: &str,
    text: &str,
    sender_color: Color,
    width: usize,
    indent: usize,
) {
    if width == 0 {
        return;
    }
    let prefix_style = Style::default()
        .fg(sender_color)
        .add_modifier(Modifier::BOLD);
    let pad = " ".repeat(indent);

    // Honor embedded newlines: each \n in the message becomes its own logical
    // paragraph, and each paragraph is then word-wrapped to the available width.
    // Only the very first segment carries the sender prefix; subsequent segments
    // are indented to align under the text portion.
    for (segment_idx, segment) in text.split('\n').enumerate() {
        if segment_idx == 0 {
            wrap_segment_with_prefix(
                lines,
                prefix,
                prefix_style,
                segment,
                width,
                &pad,
                indent,
            );
        } else {
            wrap_segment_indented(lines, segment, width, &pad, indent);
        }
    }
}

fn wrap_segment_with_prefix<'a>(
    lines: &mut Vec<Line<'a>>,
    prefix: &str,
    prefix_style: Style,
    segment: &str,
    width: usize,
    pad: &str,
    indent: usize,
) {
    let first_line_avail = width.saturating_sub(prefix.chars().count());
    let chars: Vec<char> = segment.chars().collect();

    if chars.len() <= first_line_avail {
        lines.push(Line::from(vec![
            Span::styled(prefix.to_string(), prefix_style),
            Span::raw(segment.to_string()),
        ]));
        return;
    }

    lines.push(Line::from(vec![
        Span::styled(prefix.to_string(), prefix_style),
        Span::raw(chars[..first_line_avail].iter().collect::<String>()),
    ]));

    let continuation_avail = width.saturating_sub(indent);
    if continuation_avail == 0 {
        return;
    }
    let mut pos = first_line_avail;
    while pos < chars.len() {
        let end = (pos + continuation_avail).min(chars.len());
        lines.push(Line::from(Span::raw(format!(
            "{}{}",
            pad,
            chars[pos..end].iter().collect::<String>()
        ))));
        pos = end;
    }
}

fn wrap_segment_indented<'a>(
    lines: &mut Vec<Line<'a>>,
    segment: &str,
    width: usize,
    pad: &str,
    indent: usize,
) {
    let avail = width.saturating_sub(indent);
    let chars: Vec<char> = segment.chars().collect();

    if chars.is_empty() {
        // Preserve blank lines authored by the sender (e.g. paragraph breaks).
        lines.push(Line::from(pad.to_string()));
        return;
    }

    if avail == 0 {
        return;
    }

    let mut pos = 0;
    while pos < chars.len() {
        let end = (pos + avail).min(chars.len());
        lines.push(Line::from(Span::raw(format!(
            "{}{}",
            pad,
            chars[pos..end].iter().collect::<String>()
        ))));
        pos = end;
    }
}

/// Wrap an annotation line (transcription / translation) with a single style
/// applied to the whole line. Continuation lines are indented under the text
/// portion so they align with the first character after the label. Embedded
/// newlines in `text` are preserved as line breaks.
fn wrap_styled<'a>(
    lines: &mut Vec<Line<'a>>,
    prefix: &str,
    text: &str,
    style: Style,
    width: usize,
) {
    if width == 0 {
        return;
    }
    let indent = prefix.chars().count();
    let pad = " ".repeat(indent);
    let avail = width.saturating_sub(indent);

    for (segment_idx, segment) in text.split('\n').enumerate() {
        let chars: Vec<char> = segment.chars().collect();
        let (line_pad, is_first) = if segment_idx == 0 {
            (prefix.to_string(), true)
        } else {
            (pad.clone(), false)
        };

        if chars.is_empty() {
            // Blank segment from a literal "\n\n": preserve it (use prefix on the
            // first segment, indented blank otherwise).
            lines.push(Line::from(Span::styled(line_pad, style)));
            continue;
        }

        if avail == 0 {
            return;
        }

        // First segment: first chunk includes the prefix; later chunks use pad.
        // Subsequent segments: every chunk uses pad.
        let mut pos = 0;
        let mut chunk_idx = 0;
        while pos < chars.len() {
            let end = (pos + avail).min(chars.len());
            let chunk: String = chars[pos..end].iter().collect();
            let leader = if is_first && chunk_idx == 0 {
                prefix.to_string()
            } else {
                pad.clone()
            };
            lines.push(Line::from(Span::styled(
                format!("{}{}", leader, chunk),
                style,
            )));
            pos = end;
            chunk_idx += 1;
        }
    }
}

fn help_text() -> Vec<Line<'static>> {
    vec![
        Line::from(""),
        Line::from(Span::styled(
            "  WhatsCLI - WhatsApp Terminal Client",
            Style::default()
                .fg(Color::Green)
                .add_modifier(Modifier::BOLD),
        )),
        Line::from(""),
        Line::from("  Keyboard shortcuts:"),
        Line::from(""),
        Line::from("    Tab         Switch between chat list and messages"),
        Line::from("    Ctrl+N/P    Next/previous chat"),
        Line::from("    Ctrl+B      Load older messages"),
        Line::from("    Ctrl+U      Mark chat as unread"),
        Line::from("    Ctrl+F      Search chats"),
        Line::from("    Ctrl+Q      Quit"),
        Line::from("    Up/Down     Navigate / scroll"),
        Line::from("    PgUp/PgDn   Scroll messages"),
        Line::from("    Enter       Select chat / send message"),
        Line::from("    /           Start command"),
        Line::from("    Esc         Cancel"),
        Line::from(""),
        Line::from("  Commands (prefix with /):"),
        Line::from(""),
        Line::from("    /login      Connect to WhatsApp"),
        Line::from("    /disconnect Disconnect"),
        Line::from("    /logout     Log out completely"),
        Line::from("    /backlog    Load older messages"),
        Line::from("    /read       Mark chat as read"),
        Line::from("    /unread     Mark chat as unread"),
        Line::from("    /download   Download last media"),
        Line::from("    /open       Open last media"),
        Line::from("    /upload     Upload file to chat"),
        Line::from("    /info       Message info"),
        Line::from("    /url        Open URL from message"),
        Line::from("    /revoke     Delete message"),
        Line::from("    /leave      Leave group"),
        Line::from("    /subject    Set group subject"),
    ]
}
