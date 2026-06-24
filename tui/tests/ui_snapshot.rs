use ratatui::{backend::TestBackend, Terminal};

use whatscli_tui::proto::whatscli::{self as pb, MessageKind};
use whatscli_tui::state::{App, FocusPane};
use whatscli_tui::ui;

fn make_chat(id: &str, name: &str, archived: bool) -> pb::ChatProto {
    pb::ChatProto {
        id: id.to_string(),
        name: name.to_string(),
        archived,
        ..Default::default()
    }
}

fn make_message(id: &str, text: &str, from_me: bool, contact: &str) -> pb::MessageProto {
    pb::MessageProto {
        id: id.to_string(),
        text: text.to_string(),
        from_me,
        contact_name: contact.to_string(),
        contact_short: contact.to_string(),
        chat_id: "chat1".to_string(),
        kind: MessageKind::Text as i32,
        ..Default::default()
    }
}

fn make_message_at(
    id: &str,
    text: &str,
    from_me: bool,
    contact: &str,
    timestamp: u64,
) -> pb::MessageProto {
    pb::MessageProto {
        id: id.to_string(),
        text: text.to_string(),
        from_me,
        contact_name: contact.to_string(),
        contact_short: contact.to_string(),
        chat_id: "chat1".to_string(),
        kind: MessageKind::Text as i32,
        timestamp,
        ..Default::default()
    }
}

fn make_image_message(id: &str, from_me: bool, contact: &str) -> pb::MessageProto {
    pb::MessageProto {
        id: id.to_string(),
        text: String::new(),
        from_me,
        contact_name: contact.to_string(),
        contact_short: contact.to_string(),
        chat_id: "chat1".to_string(),
        kind: MessageKind::Image as i32,
        ..Default::default()
    }
}

fn setup_app() -> App {
    let mut app = App::new();
    app.chats = vec![
        make_chat("chat1", "Alice", false),
        make_chat("chat2", "Bob", false),
        make_chat("chat3", "Carlos", false),
    ];
    app.archived_chats = vec![
        make_chat("arch1", "Old Group", true),
    ];
    app
}

fn render_to_string(app: &mut App, width: u16, height: u16) -> String {
    let backend = TestBackend::new(width, height);
    let mut terminal = Terminal::new(backend).unwrap();
    terminal.draw(|f| ui::draw(f, app)).unwrap();
    let buf = terminal.backend().buffer().clone();
    let mut output = String::new();
    for y in 0..height {
        for x in 0..width {
            let cell = &buf[(x, y)];
            output.push_str(cell.symbol());
        }
        output.push('\n');
    }
    output
}

#[test]
fn test_startup_shows_help() {
    let mut app = setup_app();
    let output = render_to_string(&mut app, 100, 30);
    assert!(output.contains("WhatsCLI"), "should show WhatsCLI title");
    assert!(output.contains("Navigation"), "should show shortcut sections");
    assert!(output.contains("Alice"), "should show chat list");
    assert!(output.contains("Bob"), "should show chat list");
    assert!(output.contains("Archived"), "should show archived folder");
}

#[test]
fn test_chat_selected_shows_messages() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![
        make_message("m1", "Hello!", false, "Alice"),
        make_message("m2", "Hi there!", true, "Me"),
        make_message("m3", "How are you?", false, "Alice"),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);
    assert!(output.contains("Alice"), "should show Alice in title or messages");
    assert!(output.contains("Hello!"), "should show first message");
    assert!(output.contains("Hi there!"), "should show second message");
    assert!(output.contains("How are you?"), "should show third message");
    assert!(!output.contains("Navigation"), "should NOT show help when chat selected");
}

/// Reproduces the user-reported readability problem: short senders ("Me",
/// "Elvi") and long senders ("50672809445:3", "Laurie-Ève") used to start
/// their message text at different columns, forcing the eye to zigzag.
/// All message text should now begin at the **same** column on every line.
#[test]
fn test_message_text_aligns_to_same_column_across_senders() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let ts = 1_700_000_000_u64;
    app.messages = vec![
        make_message_at("m1", "short sender msg", true, "Me", ts),
        make_message_at("m2", "medium sender msg", false, "Elvi", ts),
        make_message_at("m3", "long sender msg", false, "Laurie-Ève", ts),
        make_message_at("m4", "phone JID msg", false, "50672809445:3", ts),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);

    // Find the visible terminal **column** (not byte offset!) where each
    // message body starts. We have to count chars before the needle, not
    // bytes, because senders like "Laurie-Ève" and the truncation marker "…"
    // are multi-byte in UTF-8 but render as single columns in the terminal.
    let needles = ["short sender msg", "medium sender msg", "long sender msg", "phone JID msg"];
    let columns: Vec<usize> = needles
        .iter()
        .map(|needle| {
            for line in output.lines() {
                if let Some(byte_idx) = line.find(needle) {
                    return line[..byte_idx].chars().count();
                }
            }
            panic!("did not find {:?} in output:\n{}", needle, output);
        })
        .collect();

    // All four message bodies must start at the exact same column.
    assert!(
        columns.windows(2).all(|w| w[0] == w[1]),
        "message bodies start at different columns: {:?}",
        columns
    );
}

#[test]
fn test_translations_displayed() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![
        make_message("m1", "Buenos días", false, "Carlos"),
    ];
    app.translations.insert("m1".to_string(), "Good morning".to_string());
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);
    assert!(output.contains("Buenos días"), "original message shown");
    assert!(output.contains("(EN)"), "translation label shown");
    assert!(output.contains("Good morning"), "translation text shown");
}

/// The user's readability fix: when a message has a translation, the
/// translation **body** must start at the same terminal column as the
/// original message body. The "(EN)" / "[TR]" labels live in the
/// timestamp slot (where there's free space and a natural place for
/// metadata) so the eye scans a single uninterrupted column for primary
/// content text.
#[test]
fn test_translation_body_aligns_to_message_body_column() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let ts = 1_700_000_000_u64;
    // Mix of short and long sender names. Sender prefix is fixed-width so
    // message bodies align — translation bodies must do the same.
    app.messages = vec![
        make_message_at("m1", "Buenos días", false, "Elvi", ts),
        make_message_at("m2", "Hola amigo", false, "50672809445", ts),
    ];
    app.translations
        .insert("m1".to_string(), "Good morning".to_string());
    app.translations
        .insert("m2".to_string(), "Hello friend".to_string());
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);

    // Reference column: start of the first original message body.
    let body_col = column_of(&output, "Buenos días")
        .unwrap_or_else(|| panic!("missing original in:\n{output}"));

    // Sanity: both originals share the body column.
    let body_col_2 = column_of(&output, "Hola amigo")
        .unwrap_or_else(|| panic!("missing second original in:\n{output}"));
    assert_eq!(
        body_col, body_col_2,
        "originals should align via fixed-width sender prefix"
    );

    // Each translation **body** must start at exactly that same column.
    for needle in ["Good morning", "Hello friend"] {
        let body_col_tr = column_of(&output, needle)
            .unwrap_or_else(|| panic!("missing translation {needle:?} in:\n{output}"));
        assert_eq!(
            body_col_tr, body_col,
            "translation body {needle:?} starts at col {body_col_tr} but the \
             original body starts at col {body_col}; they should match"
        );
    }

    // The label must live to the *left* of the body column (in the timestamp
    // slot) — never on or past it.
    for needle in ["Good morning", "Hello friend"] {
        let line = output
            .lines()
            .find(|l| l.contains(needle))
            .unwrap_or_else(|| panic!("missing translation line for {needle:?}"));
        let label_byte = line
            .find("(EN)")
            .unwrap_or_else(|| panic!("translation line {line:?} should contain (EN) label"));
        let label_col = line[..label_byte].chars().count();
        assert!(
            label_col < body_col,
            "label should sit left of body (col {label_col} < {body_col}); got {line:?}"
        );
    }
}

/// Helper: find the visible terminal column where `needle` first appears in
/// the rendered output. Counts chars (i.e. display cells for our test
/// strings, none of which use multi-column glyphs in the body), not bytes.
fn column_of(output: &str, needle: &str) -> Option<usize> {
    for line in output.lines() {
        if let Some(byte_idx) = line.find(needle) {
            return Some(line[..byte_idx].chars().count());
        }
    }
    None
}

#[test]
fn test_image_placeholder() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![
        make_message("m1", "Check this out:", false, "Alice"),
        make_image_message("m2", false, "Alice"),
        make_message("m3", "Nice!", true, "Me"),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);
    assert!(output.contains("[IMAGE]"), "should show [IMAGE] placeholder");
    assert!(output.contains("Check this out:"), "text before image shown");
    assert!(output.contains("Nice!"), "text after image shown");
}

#[test]
fn test_scroll_offset_normalized() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![
        make_message("m1", "Message 1", false, "Alice"),
        make_message("m2", "Message 2", false, "Alice"),
        make_message("m3", "Message 3", false, "Alice"),
    ];
    app.scroll_offset = usize::MAX;

    render_to_string(&mut app, 100, 30);
    assert!(app.scroll_offset < usize::MAX, "scroll_offset should be normalized after render");
}

#[test]
fn test_scroll_up_changes_view() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let mut msgs = Vec::new();
    for i in 0..100 {
        msgs.push(make_message(
            &format!("m{}", i),
            &format!("Message number {}", i),
            i % 2 == 0,
            "Alice",
        ));
    }
    app.messages = msgs;
    app.scroll_offset = usize::MAX;

    let before = render_to_string(&mut app, 100, 20);
    assert!(before.contains("Message number 99"), "should show last message at bottom");

    app.scroll_up(10);
    let after = render_to_string(&mut app, 100, 20);
    assert!(!after.contains("Message number 99"), "should NOT show last message after scroll up");
    assert!(after.contains("Message number 89") || after.contains("Message number 85"),
        "should show earlier messages after scroll up");
}

#[test]
fn test_focus_highlight() {
    let mut app = setup_app();

    let backend = TestBackend::new(100, 30);
    let mut terminal = Terminal::new(backend).unwrap();

    app.focus = FocusPane::ChatList;
    terminal.draw(|f| ui::draw(f, &mut app)).unwrap();
    let buf_chat = terminal.backend().buffer().clone();

    app.focus = FocusPane::Messages;
    terminal.draw(|f| ui::draw(f, &mut app)).unwrap();
    let buf_msg = terminal.backend().buffer().clone();

    let mut style_differs = false;
    for y in 0..30 {
        for x in 0..100 {
            if buf_chat[(x, y)].style() != buf_msg[(x, y)].style() {
                style_differs = true;
                break;
            }
        }
        if style_differs { break; }
    }
    assert!(style_differs, "focus change should affect border styles");
}

#[test]
fn test_empty_chat_list() {
    let mut app = App::new();
    let output = render_to_string(&mut app, 100, 30);
    assert!(output.contains("WhatsCLI"), "should still show title");
    assert!(output.contains("Navigation"), "should show help with shortcut sections");
}

#[test]
fn test_long_message_wraps() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let long_msg = "A".repeat(200);
    app.messages = vec![
        make_message("m1", &long_msg, false, "Alice"),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 80, 20);
    let alice_lines: Vec<&str> = output.lines().filter(|l| l.contains('A')).collect();
    assert!(alice_lines.len() > 1, "long message should wrap to multiple lines");
}

/// Reproduces the user-reported bug where wrapping cut words mid-character.
///
/// In a narrow viewport the long message used to render as e.g.:
///   "...are shouting at the same time wh"
///   "en we are trying to..."
/// With word-aware wrapping, no whole word should ever be split unless it's
/// genuinely longer than the available width.
#[test]
fn test_message_wraps_at_word_boundaries_not_mid_word() {
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let text =
        "I've to admit that the stress level of the pick up time can rise quickly when two \
         or three kids are shouting at the same time when we are trying to spot other \
         parents to get their approval";
    app.messages = vec![make_message("m1", text, false, "Laurie")];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 70, 20);

    // For every line in the message body, find the trailing token. If it
    // doesn't end the message, it must be a complete word from the original
    // text (i.e. it must appear as a standalone whitespace-delimited token in
    // the source). This catches the "wh"/"en" split.
    let original_words: std::collections::HashSet<&str> = text.split_whitespace().collect();
    let mut body_lines: Vec<String> = output
        .lines()
        .filter(|l| {
            let trimmed = l.trim();
            !trimmed.is_empty()
                && !trimmed.starts_with('│')
                && !trimmed.starts_with('└')
                && !trimmed.starts_with('┌')
                && !trimmed.starts_with('─')
        })
        .filter_map(|l| {
            // Strip the panel-border vertical bars on each side.
            let s = l.trim_matches(|c: char| c == '│' || c == ' ');
            if s.is_empty() { None } else { Some(s.to_string()) }
        })
        .filter(|l| {
            // Only the lines that actually carry message content (and not
            // chat-list rows, the day divider, or status bar).
            l.contains("Laurie") || original_words.iter().any(|w| l.contains(w))
        })
        .collect();

    // Drop the last line of the message - it can legitimately end on any word.
    if body_lines.len() > 1 {
        body_lines.pop();
    }

    for line in &body_lines {
        let last = line.split_whitespace().next_back().unwrap_or("");
        // A trailing token is acceptable if it appears verbatim in the source
        // text as a complete word. The old buggy slicer would produce "wh",
        // "en", etc. which are NOT in the source.
        assert!(
            original_words.contains(last) || last.is_empty(),
            "wrapped line ends with partial word {:?}: full line = {:?}",
            last,
            line,
        );
    }
}

#[test]
fn test_multiline_message_preserves_newlines() {
    // Reproduces the user-reported bug: a message authored with embedded
    // newlines (e.g. via Shift+Enter in the composer) was rendered as a single
    // line in the chat view because the wrap logic ignored '\n'.
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![
        make_message(
            "m1",
            "this is only\na test of a multi-line\nmessage",
            true,
            "Me",
        ),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 20);

    // Each segment must appear on its own visible line. The first segment is
    // co-located with the "Me" sender prefix (now padded to a fixed width for
    // alignment); the others are indented to match.
    let me_line = output
        .lines()
        .find(|l| l.contains("Me") && l.contains("this is only"))
        .expect("first segment should be on the prefix line");
    let second_line = output
        .lines()
        .find(|l| l.contains("a test of a multi-line"))
        .expect("second segment should be on its own line");
    let third_line = output
        .lines()
        .find(|l| {
            // Skip the sender-prefix line that already contains "this is only".
            l.contains("message")
                && !l.contains("this is only")
                && !l.contains("a test")
        })
        .expect("third segment should be on its own line");

    // None of the segments should be smushed together by stripped newlines.
    assert!(
        !me_line.contains("a test of a multi-line"),
        "first and second segments must not collapse onto one line, got: {:?}",
        me_line
    );
    assert!(
        !second_line.contains("message"),
        "second and third segments must not collapse onto one line, got: {:?}",
        second_line
    );
    assert!(
        third_line.contains("message"),
        "expected the third segment line to contain 'message', got: {:?}",
        third_line
    );
}

#[test]
fn test_message_shows_inline_hhmm_timestamp() {
    // A message authored at 2026-04-20 17:05 UTC (Costa Rica local 11:05)
    // should show its HH:MM next to the sender. We can't reliably assert the
    // local-time string because the host timezone is unknown, but we *can*
    // verify the rendered line contains something that looks like HH:MM.
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let ts = 1_777_000_000u64; // 2026-04-20 17:06:40 UTC
    app.messages = vec![make_message_at("m1", "hi there", false, "Alice", ts)];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 20);
    let alice_line = output
        .lines()
        .find(|l| l.contains("Alice") && l.contains("hi there"))
        .expect("message line should be rendered");

    // Confirm a clock of the form HH:MM sits between the sender and the body.
    // Hand-rolled check to avoid pulling in a regex dependency.
    let after_alice = alice_line
        .split_once("Alice")
        .map(|(_, rest)| rest.trim_start())
        .expect("line should contain 'Alice'");
    let clock: String = after_alice.chars().take(5).collect();
    let bytes = clock.as_bytes();
    assert!(
        bytes.len() == 5
            && bytes[0].is_ascii_digit()
            && bytes[1].is_ascii_digit()
            && bytes[2] == b':'
            && bytes[3].is_ascii_digit()
            && bytes[4].is_ascii_digit(),
        "expected 'HH:MM' after sender, got {:?} from line {:?}",
        clock,
        alice_line
    );
    assert!(
        after_alice.contains("hi there"),
        "expected body text after clock, got: {:?}",
        alice_line
    );
}

/// Counts how many day-divider lines appear in `rendered`. A divider is any
/// line that contains at least one `─ ` (dash, space) AND ` ─` (space, dash),
/// which excludes plain top/bottom panel borders (those are solid runs of `─`
/// broken only by panel corner characters).
fn count_day_dividers(rendered: &str) -> usize {
    rendered
        .lines()
        .filter(|l| l.contains("─ ") && l.contains(" ─"))
        .count()
}

#[test]
fn test_day_divider_separates_different_days() {
    // Two messages from different local calendar days should have a day
    // divider inserted between them. We use timestamps that are ~48 hours
    // apart in UTC, which guarantees a day boundary regardless of offset.
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let ts_old = 1_776_800_000u64; // earlier day
    let ts_new = ts_old + 2 * 24 * 3600; // two days later
    app.messages = vec![
        make_message_at("m1", "first message", false, "Alice", ts_old),
        make_message_at("m2", "second message", false, "Alice", ts_new),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);

    // Two dividers total: one before the first message, one before the second.
    let divider_count = count_day_dividers(&output);
    assert!(
        divider_count >= 2,
        "expected at least 2 day dividers, got {}: {}",
        divider_count,
        output
    );

    // The older message is under one divider, the newer under another, and
    // the dividers and messages appear in the right order (older -> newer).
    let idx_first = output
        .lines()
        .position(|l| l.contains("first message"))
        .expect("first message should render");
    let idx_second = output
        .lines()
        .position(|l| l.contains("second message"))
        .expect("second message should render");
    assert!(idx_first < idx_second, "older message should render above newer");
}

#[test]
fn test_day_divider_not_duplicated_for_same_day() {
    // Three messages on the same local day should produce exactly one divider.
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    let base = 1_776_800_000u64;
    app.messages = vec![
        make_message_at("m1", "msg one", false, "Alice", base),
        make_message_at("m2", "msg two", false, "Alice", base + 60),
        make_message_at("m3", "msg three", false, "Alice", base + 120),
    ];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 30);
    let divider_count = count_day_dividers(&output);
    assert_eq!(
        divider_count, 1,
        "expected exactly 1 divider for same-day messages, got {}: {}",
        divider_count, output
    );
}

#[test]
fn test_help_modal_renders_when_show_help_true() {
    // The modal popup should overlay the rest of the UI and contain the
    // section headings + the close-key hint in its title. Use a tall
    // viewport so the popup isn't clamped by the 90% height cap.
    let mut app = setup_app();
    app.show_help = true;

    let output = render_to_string(&mut app, 120, 80);

    assert!(
        output.contains("Help"),
        "modal title 'Help' should be visible: {}",
        output
    );
    assert!(
        output.contains("Esc / ? / q to close"),
        "close-key hint should be visible: {}",
        output
    );
    assert!(
        output.contains("Navigation"),
        "Navigation section should appear: {}",
        output
    );
    assert!(
        output.contains("Ctrl+I"),
        "Ctrl+I shortcut should be listed: {}",
        output
    );
    assert!(
        output.contains("Slash-commands"),
        "Slash-commands section should appear: {}",
        output
    );
}

#[test]
fn test_help_modal_hidden_by_default() {
    let mut app = setup_app();
    let output = render_to_string(&mut app, 120, 40);
    assert!(
        !output.contains("Esc / ? / q to close"),
        "modal must not render when show_help is false: {}",
        output
    );
}

#[test]
fn test_divider_skipped_for_missing_timestamp() {
    // Legacy messages without timestamps must not produce a bogus
    // "Thu, Jan 1 1970" divider from a zero timestamp.
    let mut app = setup_app();
    app.current_chat = Some("chat1".to_string());
    app.messages = vec![make_message("m1", "no timestamp here", true, "Me")];
    app.scroll_offset = usize::MAX;

    let output = render_to_string(&mut app, 100, 20);
    assert!(
        !output.contains("1970"),
        "no-timestamp messages must not emit a 1970 divider, got: {}",
        output
    );
    assert_eq!(
        count_day_dividers(&output),
        0,
        "no-timestamp messages must not emit a divider at all, got: {}",
        output
    );
}
