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
    assert!(output.contains("Keyboard shortcuts"), "should show help text");
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
    assert!(!output.contains("Keyboard shortcuts"), "should NOT show help when chat selected");
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
    assert!(output.contains("Carlos(EN)"), "translation label shown");
    assert!(output.contains("Good morning"), "translation text shown");
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
    assert!(output.contains("Keyboard shortcuts"), "should show help");
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
    // co-located with the "Me: " sender prefix, the others are indented.
    let me_line = output
        .lines()
        .find(|l| l.contains("Me:") && l.contains("this is only"))
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
                && !l.contains("Me:")
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
