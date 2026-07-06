use ratatui::{
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Paragraph},
    Frame,
};
use ratatui_image::StatefulImage;
use time::OffsetDateTime;

use crate::proto::whatscli::MessageKind;
use crate::state::{App, FocusPane};
use crate::ui::timestamps::{current_local_offset, format_clock, format_day_label, local_day_key};
use crate::ui::wrap::{display_width, wrap_words};

const IMAGE_MAX_HEIGHT: u16 = 20;
const IMAGE_MIN_HEIGHT: u16 = 3;

/// Width (in terminal columns) of the sender-name field at the start of every
/// message line. All sender names are truncated or right-padded to this width
/// so that the `HH:MM` clock and the message text always start at the same
/// column, no matter who sent the message. This makes the chat scannable -
/// your eye can read straight down a single column instead of zigzagging.
///
/// Chosen to fit common WhatsApp short names (`Anton`, `Elvi`, `Laurie-Ève`)
/// and most phone-JID display strings (`50672809445`) without truncation,
/// while still leaving plenty of room for message text in a typical
/// 80-column viewport.
const SENDER_FIELD_WIDTH: usize = 12;

/// The character appended when truncating a sender name. One column wide.
const TRUNCATION_MARKER: char = '…';

/// Width (in terminal columns) of the clock slot in the message prefix. The
/// clock is `HH:MM` in 24-hour format, which is invariant 5 columns wide.
/// Annotation prefixes (translation/transcription labels) reuse this slot
/// so their bodies align under the message body above them.
const CLOCK_SLOT_WIDTH: usize = 5;

/// Number of spaces between the clock slot and the body text. Used by both
/// message and annotation prefixes so the body always lands at the same
/// column.
const BODY_GAP: usize = 2;

/// Total width of any message-or-annotation prefix:
///
///   {sender field} {space} {clock slot} {body gap}
///
/// Centralised so message wrapping and annotation wrapping can't drift apart.
const PREFIX_WIDTH: usize = SENDER_FIELD_WIDTH + 1 + CLOCK_SLOT_WIDTH + BODY_GAP;

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
        let help = crate::ui::help::help_lines();
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

    // Track [start, end) line ranges per message so we can post-process the
    // cursored message's lines (highlight background) without complicating
    // the per-message rendering loop. Indexed by the position of the
    // message in `app.messages`.
    let mut message_line_ranges: Vec<(usize, usize)> = Vec::with_capacity(app.messages.len());

    // Compute once per frame: local timezone offset and the reference "now"
    // used for "Today"/"Yesterday" labelling.
    let offset = current_local_offset();
    let now = OffsetDateTime::now_utc();
    let mut last_day_key: Option<(i32, u16)> = None;

    for (msg_idx, msg) in app.messages.iter().enumerate() {
        let is_cursored = app.message_cursor == Some(msg_idx);
        let msg_start = lines.len();
        // Day-boundary divider (Slack-style). Only emit when the message has a
        // real timestamp - defensive against backend bugs that would produce
        // spurious "Thu, Jan 1 1970" dividers.
        if msg.timestamp > 0 {
            let key = local_day_key(msg.timestamp, offset);
            if last_day_key != Some(key) {
                push_day_divider(&mut lines, msg.timestamp, now, offset, inner_width);
                last_day_key = Some(key);
            }
        }

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

        let (prefix_spans, prefix_width) = build_message_prefix(&sender, msg.timestamp, sender_color, offset);

        if text.is_empty() && image_loaded {
            lines.push(Line::from(prefix_spans));
        } else {
            wrap_message(&mut lines, prefix_spans, &text, inner_width, prefix_width);
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
            wrap_annotation(&mut lines, "[TR]", transcription, dim_style, inner_width);
            // Copy affordance: a long voice-note transcript is painful to
            // select with the mouse, so surface a one-key copy hint directly
            // beneath the transcript of the *selected* message. Pressing `c`
            // (see `handle_attachment_shortcut`) copies it to the clipboard.
            if is_cursored {
                push_copy_hint(&mut lines, inner_width);
            }
        }

        if let Some(translation) = app.translations.get(&msg.id) {
            wrap_annotation(&mut lines, "(EN)", translation, dim_style, inner_width);
        }

        // Persistent inline annotation for files the user saved this
        // session. Lives directly under the message it belongs to so
        // there's no question what was saved where, and stays for the
        // whole session (the toast at the top is transient).
        if let Some(path) = app.saved_files.get(&msg.id) {
            wrap_annotation(
                &mut lines,
                "→",
                &format!("saved to {}", path),
                dim_style,
                inner_width,
            );
        }

        message_line_ranges.push((msg_start, lines.len()));
    }

    // Apply cursor highlight to all lines belonging to the cursored
    // message. Done as a post-pass so the per-message rendering loop
    // doesn't have to thread highlight state through every helper.
    let cursored_range: Option<(usize, usize)> = app
        .message_cursor
        .and_then(|i| message_line_ranges.get(i).copied());
    if let Some((start, end)) = cursored_range {
        let highlight_bg = Color::Rgb(40, 40, 60);
        for line in &mut lines[start..end] {
            for span in &mut line.spans {
                span.style = span.style.bg(highlight_bg);
            }
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

    // Ensure the cursored message is visible. Without this, holding Up
    // walks the cursor off the top of the viewport silently — the
    // highlight is invisible above the visible area, and "Up/Down"
    // appears to do nothing until the cursor finally hits the top of
    // history and triggers the at-edge scroll fall-through. Equivalent
    // problem on Down + scroll-up history.
    //
    // We adjust scroll_offset so the cursored message's line range fits
    // inside the visible viewport rows. If the cursor is above the
    // viewport, scroll up to put its first line at the top; if below,
    // scroll down to put its last line at the bottom. Also disengages
    // follow_tail so a chase-the-cursor scroll doesn't immediately get
    // overridden by the tail-pin on the next frame.
    if let Some((cur_start, cur_end)) = cursored_range {
        let viewport_top = app.scroll_offset;
        let viewport_bottom = viewport_top + visible as usize;
        let last_line = cur_end.saturating_sub(1);

        if cur_start < viewport_top {
            app.scroll_offset = cur_start;
            app.follow_tail = false;
        } else if last_line >= viewport_bottom {
            app.scroll_offset = (last_line + 1).saturating_sub(visible as usize);
            // Re-pin tail only if the new offset happens to land at the bottom.
            app.follow_tail = app.scroll_offset >= max_scroll;
        }
        app.scroll_offset = app.scroll_offset.min(max_scroll);
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

/// Build the leading spans for a message line.
///
/// Layout (every message gets the same fixed-width prefix so the clock and the
/// message text always start at the same column):
///
///   {sender, fitted to SENDER_FIELD_WIDTH cols} {space} {HH:MM, dim} {2 spaces}
///
/// Senders shorter than `SENDER_FIELD_WIDTH` are right-padded with spaces.
/// Senders longer are truncated and given a trailing `…`. The returned width
/// is the total visible column count of the prefix; callers use it as the
/// wrap-continuation indent so that wrapped lines align under the start of
/// the message text.
fn build_message_prefix<'a>(
    sender: &str,
    timestamp: u64,
    sender_color: Color,
    offset: time::UtcOffset,
) -> (Vec<Span<'a>>, usize) {
    let sender_style = Style::default()
        .fg(sender_color)
        .add_modifier(Modifier::BOLD);
    let time_style = Style::default()
        .fg(Color::White)
        .add_modifier(Modifier::DIM);

    let fitted_sender = fit_to_width(sender, SENDER_FIELD_WIDTH);

    if timestamp == 0 {
        // Backward-compat: no timestamp available. We still emit a full
        // PREFIX_WIDTH-wide leader so the message text starts at the same
        // column as timestamped messages in the same chat. Pad the clock
        // slot with spaces.
        let pad_after_sender = " ".repeat(1 + CLOCK_SLOT_WIDTH + BODY_GAP);
        return (
            vec![
                Span::styled(fitted_sender, sender_style),
                Span::raw(pad_after_sender),
            ],
            PREFIX_WIDTH,
        );
    }

    // Fit the clock to CLOCK_SLOT_WIDTH so any future format change (e.g.
    // a 4-char "9:30" edge case) doesn't shift the body column.
    let clock = fit_to_width(&format_clock(timestamp, offset), CLOCK_SLOT_WIDTH);
    let spans = vec![
        Span::styled(fitted_sender, sender_style),
        Span::raw(" ".to_string()),
        Span::styled(clock, time_style),
        Span::raw(" ".repeat(BODY_GAP)),
    ];
    (spans, PREFIX_WIDTH)
}

/// Build the leader for a translation / transcription line.
///
/// Same column layout as `build_message_prefix`, but with the sender slot
/// blanked and the `label` placed in the clock slot. This makes annotation
/// bodies start at exactly the same terminal column as the original message
/// body above them, so the eye scans down a single column for primary text.
///
/// `label` is fitted to `CLOCK_SLOT_WIDTH` (truncated with `…` if too long,
/// right-padded with spaces if shorter). The whole leader is styled `style`.
fn build_annotation_prefix<'a>(label: &str, style: Style) -> (Vec<Span<'a>>, usize) {
    let blank_sender = " ".repeat(SENDER_FIELD_WIDTH);
    let fitted_label = fit_to_width(label, CLOCK_SLOT_WIDTH);
    let spans = vec![
        Span::styled(blank_sender, style),
        Span::raw(" ".to_string()),
        Span::styled(fitted_label, style),
        Span::raw(" ".repeat(BODY_GAP)),
    ];
    (spans, PREFIX_WIDTH)
}

/// Truncate or right-pad `s` so it occupies exactly `width` terminal columns.
///
/// If `s` is wider than `width`, characters are dropped from the end and
/// replaced with `TRUNCATION_MARKER` (which is one column wide). Otherwise
/// the string is right-padded with ASCII spaces. Truncation is done in terms
/// of column width so emoji and CJK characters (2 columns each) are handled
/// correctly.
fn fit_to_width(s: &str, width: usize) -> String {
    let s_width = display_width(s);
    if s_width == width {
        return s.to_string();
    }
    if s_width < width {
        let mut out = String::with_capacity(s.len() + (width - s_width));
        out.push_str(s);
        for _ in 0..(width - s_width) {
            out.push(' ');
        }
        return out;
    }

    // Truncate: take chars greedily up to `width - 1` columns, then append the
    // ellipsis marker so the total is exactly `width`.
    if width == 0 {
        return String::new();
    }
    let target = width.saturating_sub(1);
    let mut out = String::new();
    let mut col: usize = 0;
    for c in s.chars() {
        let cw = unicode_width::UnicodeWidthChar::width(c).unwrap_or(0);
        if col + cw > target {
            break;
        }
        out.push(c);
        col += cw;
    }
    // Pad with a space if a 2-column char tipped us short of `width - 1`.
    if col < target {
        for _ in 0..(target - col) {
            out.push(' ');
        }
    }
    out.push(TRUNCATION_MARKER);
    out
}

/// Push a Slack-style day-boundary divider spanning the available width.
fn push_day_divider<'a>(
    lines: &mut Vec<Line<'a>>,
    timestamp: u64,
    now: OffsetDateTime,
    offset: time::UtcOffset,
    width: usize,
) {
    let label = format_day_label(timestamp, now, offset);
    // Frame the label between dashes centered horizontally:
    //   "─── Today ─────────────────────────────"
    // Build visually; the " label " bit is bold-dim, the dashes dim-gray.
    let padded = format!(" {} ", label);
    let padded_len = display_width(&padded);

    let total_dashes = width.saturating_sub(padded_len);
    let left = total_dashes / 2;
    let right = total_dashes - left;

    let dash_style = Style::default()
        .fg(Color::DarkGray);
    let label_style = Style::default()
        .fg(Color::Gray)
        .add_modifier(Modifier::BOLD);

    // Add a blank line above each divider so they visually breathe.
    lines.push(Line::from(""));
    lines.push(Line::from(vec![
        Span::styled("─".repeat(left), dash_style),
        Span::styled(padded, label_style),
        Span::styled("─".repeat(right), dash_style),
    ]));
}

/// Render a message's sender prefix + text into `lines`, wrapping to `width`.
///
/// The first line starts with the given `prefix_spans` (already styled). Every
/// subsequent wrapped line, including continuations from embedded `\n`s in the
/// text, is indented to `indent` columns so it aligns under the start of the
/// message text (not under the sender name). `indent` must equal the total
/// visible width of `prefix_spans`.
///
/// Wrapping is word-aware (see `crate::ui::wrap::wrap_words`): we never split
/// a word across lines unless it's longer than the available width.
fn wrap_message<'a>(
    lines: &mut Vec<Line<'a>>,
    prefix_spans: Vec<Span<'a>>,
    text: &str,
    width: usize,
    indent: usize,
) {
    if width == 0 {
        return;
    }
    let pad = " ".repeat(indent);
    let avail = width.saturating_sub(indent);
    if avail == 0 {
        return;
    }

    let mut emitted_first_line = false;

    for (segment_idx, segment) in text.split('\n').enumerate() {
        if segment.is_empty() {
            // Author put a literal newline (or two in a row): preserve the
            // blank line so paragraph breaks round-trip.
            if segment_idx == 0 {
                // Empty first segment: still need to render the prefix so the
                // sender name doesn't disappear for, e.g., a media-only msg.
                lines.push(Line::from(prefix_spans.clone()));
                emitted_first_line = true;
            } else {
                lines.push(Line::from(pad.clone()));
            }
            continue;
        }

        let wrapped = wrap_words(segment, avail);
        for (chunk_idx, chunk) in wrapped.into_iter().enumerate() {
            if !emitted_first_line && segment_idx == 0 && chunk_idx == 0 {
                let mut spans = prefix_spans.clone();
                spans.push(Span::raw(chunk));
                lines.push(Line::from(spans));
                emitted_first_line = true;
            } else {
                lines.push(Line::from(Span::raw(format!("{}{}", pad, chunk))));
            }
        }
    }
}

/// Push a dim, one-key copy hint aligned under an annotation body. Shown only
/// for the cursored message's transcript so the chat isn't cluttered with a
/// hint on every voice note — it behaves like a hover affordance that appears
/// on the selected row. The leading glyph reads as a "copy" icon; the text
/// spells out the key so the shortcut is discoverable without opening help.
fn push_copy_hint<'a>(lines: &mut Vec<Line<'a>>, width: usize) {
    if width <= PREFIX_WIDTH {
        // No room for the body column; skip rather than wrap the hint oddly.
        return;
    }
    let style = Style::default()
        .fg(Color::Cyan)
        .add_modifier(Modifier::DIM);
    lines.push(Line::from(vec![
        Span::raw(" ".repeat(PREFIX_WIDTH)),
        Span::styled("\u{2398} press c to copy transcript", style),
    ]));
}

/// Render an annotation (transcription / translation) attached to the message
/// above it, aligned so that the annotation's body text starts at the same
/// terminal column as the original message's body text.
///
/// Layout per line:
///
///   {`message_text_column` spaces} {label, e.g. "(EN) "} {wrapped chunk}
///
/// Continuation lines indent past the label too, so the body text reads as
/// a single left-aligned column. The whole line gets `style` applied (used
/// for the dim styling that distinguishes annotations from primary content).
///
/// The label (`(EN)`, `[TR]`, …) is placed in the *clock slot* of the prefix
/// — the same column the timestamp occupies on primary message lines — so
/// the annotation body lands at exactly the same terminal column as the
/// original message body above it. Continuation lines indent to that same
/// body column.
fn wrap_annotation<'a>(
    lines: &mut Vec<Line<'a>>,
    label: &str,
    text: &str,
    style: Style,
    width: usize,
) {
    if width == 0 {
        return;
    }
    // Body always starts at PREFIX_WIDTH; that's the whole point.
    let avail = width.saturating_sub(PREFIX_WIDTH);
    if avail == 0 {
        // Pathological narrow viewport: emit just the label so users still
        // see that the annotation exists, even if the body won't fit.
        let (prefix_spans, _) = build_annotation_prefix(label, style);
        lines.push(Line::from(prefix_spans));
        return;
    }

    let cont_pad = " ".repeat(PREFIX_WIDTH);
    let mut emitted_prefix = false;

    for (segment_idx, segment) in text.split('\n').enumerate() {
        if segment.is_empty() {
            // Preserve blank segments from "\n\n". The first blank still gets
            // the labelled prefix so the annotation never silently opens with
            // a blank line; subsequent blanks are pure indentation.
            if !emitted_prefix && segment_idx == 0 {
                let (prefix_spans, _) = build_annotation_prefix(label, style);
                lines.push(Line::from(prefix_spans));
                emitted_prefix = true;
            } else {
                lines.push(Line::from(Span::styled(cont_pad.clone(), style)));
            }
            continue;
        }

        let wrapped = wrap_words(segment, avail);
        for chunk in wrapped {
            let mut spans: Vec<Span<'a>> = if !emitted_prefix {
                emitted_prefix = true;
                build_annotation_prefix(label, style).0
            } else {
                vec![Span::styled(cont_pad.clone(), style)]
            };
            spans.push(Span::styled(chunk, style));
            lines.push(Line::from(spans));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ui::wrap::display_width as dw;

    #[test]
    fn fit_pads_short_string() {
        assert_eq!(fit_to_width("Me", 12), "Me          ");
        assert_eq!(dw(&fit_to_width("Me", 12)), 12);
    }

    #[test]
    fn fit_returns_unchanged_when_already_at_width() {
        let s = "ExactlyTwelv"; // 12 chars / 12 cols
        assert_eq!(fit_to_width(s, 12), s);
    }

    #[test]
    fn fit_truncates_long_string_with_ellipsis() {
        let out = fit_to_width("50672809445:3", 12);
        assert_eq!(dw(&out), 12);
        assert!(out.ends_with('…'), "{:?} should end with ellipsis", out);
        assert!(out.starts_with("50672809445"), "{:?}", out);
    }

    #[test]
    fn fit_handles_emoji_correctly() {
        // "💎" is 2 columns wide. 5 emoji = 10 cols, fits in 12 -> right-pad.
        let out = fit_to_width("💎💎💎💎💎", 12);
        assert_eq!(dw(&out), 12);
        // 6 emoji = 12 cols, fits exactly.
        let out2 = fit_to_width("💎💎💎💎💎💎", 12);
        assert_eq!(dw(&out2), 12);
        assert_eq!(out2, "💎💎💎💎💎💎");
        // 7 emoji = 14 cols, exceeds 12 -> truncate with ellipsis.
        let out3 = fit_to_width("💎💎💎💎💎💎💎", 12);
        assert_eq!(dw(&out3), 12, "{:?} ({} cols)", out3, dw(&out3));
        assert!(out3.ends_with('…'));
    }

    #[test]
    fn fit_empty_pads_to_full_width() {
        assert_eq!(fit_to_width("", 12), "            ");
    }

    #[test]
    fn fit_zero_width_returns_empty() {
        assert_eq!(fit_to_width("anything", 0), "");
    }

    #[test]
    fn build_prefix_aligns_short_and_long_senders_to_same_column() {
        let offset = time::UtcOffset::UTC;
        let ts = 1_700_000_000_u64;

        let (_short_spans, short_w) = build_message_prefix("Me", ts, Color::Blue, offset);
        let (_long_spans, long_w) = build_message_prefix("50672809445:3", ts, Color::Green, offset);
        let (_med_spans, med_w) = build_message_prefix("Laurie-Ève", ts, Color::Green, offset);

        assert_eq!(
            short_w, long_w,
            "short and long senders should produce identical prefix widths so the\n\
             message text starts at the same column for both"
        );
        assert_eq!(short_w, med_w);
    }

    #[test]
    fn build_prefix_emits_truncation_marker_for_overlong_sender() {
        let offset = time::UtcOffset::UTC;
        let (spans, _w) =
            build_message_prefix("ThisNameIsWayTooLong", 1, Color::Green, offset);
        // The first span is the (fitted) sender. It must end in the ellipsis
        // marker since the input is longer than SENDER_FIELD_WIDTH.
        let first = spans.first().expect("at least one span").content.as_ref();
        assert!(
            first.ends_with('…'),
            "expected truncation ellipsis in {:?}",
            first
        );
        assert_eq!(dw(first), SENDER_FIELD_WIDTH);
    }

    #[test]
    fn wrap_annotation_places_label_in_clock_slot_and_body_at_prefix_width() {
        // First emitted line should look like:
        //   "{SENDER_FIELD_WIDTH spaces} {label fitted to CLOCK_SLOT_WIDTH}{BODY_GAP spaces}Good morning"
        // i.e. the label occupies the same column the timestamp would, and
        // the body starts at PREFIX_WIDTH — same column as the message body
        // above it.
        let mut lines: Vec<Line> = Vec::new();
        wrap_annotation(&mut lines, "(EN)", "Good morning", Style::default(), 80);
        let first = render_line(&lines[0]);
        assert!(
            first.starts_with(&" ".repeat(SENDER_FIELD_WIDTH)),
            "sender slot should be blank; got {first:?}"
        );
        // Body must start exactly at PREFIX_WIDTH.
        assert_eq!(
            &first[PREFIX_WIDTH..],
            "Good morning",
            "body should start at PREFIX_WIDTH; got {first:?}"
        );
        // Label "(EN)" must sit at column SENDER_FIELD_WIDTH+1 (clock slot).
        let clock_slot_start = SENDER_FIELD_WIDTH + 1;
        assert_eq!(
            &first[clock_slot_start..clock_slot_start + 4],
            "(EN)",
            "label should sit in the clock slot; got {first:?}"
        );
    }

    #[test]
    fn wrap_annotation_continuation_lines_align_under_body() {
        // Force a wrap. With width=PREFIX_WIDTH+10, body has 10 cols per line,
        // so a 30-char string wraps. Continuation lines must indent to exactly
        // PREFIX_WIDTH (no re-emitted label) so body text lines up vertically
        // with the original message body above.
        let mut lines: Vec<Line> = Vec::new();
        wrap_annotation(
            &mut lines,
            "(EN)",
            "alpha bravo charlie delta echo",
            Style::default(),
            PREFIX_WIDTH + 10,
        );
        assert!(lines.len() >= 2, "expected wrapping; got {} lines", lines.len());
        let cont = render_line(&lines[1]);
        assert!(
            cont.starts_with(&" ".repeat(PREFIX_WIDTH)),
            "continuation should indent to PREFIX_WIDTH; got {cont:?}"
        );
        // The first body char of the continuation line must be a real word
        // char, not whitespace and not the label's '('.
        let body_start = &cont[PREFIX_WIDTH..];
        assert!(
            !body_start.starts_with('(') && !body_start.starts_with(' '),
            "continuation body should be plain text; got {cont:?}"
        );
    }

    /// Concatenate every span in a line back into a plain string for
    /// substring/prefix assertions. Strips styling but preserves whitespace.
    fn render_line(line: &Line) -> String {
        line.spans
            .iter()
            .map(|s| s.content.as_ref())
            .collect::<String>()
    }
}

