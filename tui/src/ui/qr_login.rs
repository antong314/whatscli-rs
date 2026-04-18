use ratatui::{
    layout::{Constraint, Direction, Layout, Rect},
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Paragraph, Wrap},
    Frame,
};

pub fn render(f: &mut Frame, area: Rect, qr_text: &str) {
    let block = Block::default()
        .borders(Borders::ALL)
        .title(" Login - Scan QR Code ")
        .border_style(Style::default().fg(Color::Yellow));

    let inner = block.inner(area);
    f.render_widget(block, area);

    let layout = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(2),
            Constraint::Min(1),
            Constraint::Length(2),
        ])
        .split(inner);

    let header = Paragraph::new(Line::from(Span::styled(
        "Scan this QR code with your WhatsApp app:",
        Style::default()
            .fg(Color::Yellow)
            .add_modifier(Modifier::BOLD),
    )));
    f.render_widget(header, layout[0]);

    let qr_lines = render_qr_unicode(qr_text, inner.width as usize);
    let qr_para = Paragraph::new(qr_lines).wrap(Wrap { trim: false });
    f.render_widget(qr_para, layout[1]);

    let footer = Paragraph::new(Line::from(Span::styled(
        "Waiting for scan...",
        Style::default().fg(Color::DarkGray),
    )));
    f.render_widget(footer, layout[2]);
}

fn render_qr_unicode(data: &str, _max_width: usize) -> Vec<Line<'static>> {
    // The QR code text from the backend is just the QR code data string.
    // We use a simple text display. A full implementation would use a QR
    // library to render it to unicode blocks. For now, show the code string
    // and instructions.
    vec![
        Line::from(""),
        Line::from(Span::styled(
            format!("  QR Data: {}", &data[..data.len().min(50)]),
            Style::default().fg(Color::White),
        )),
        Line::from(""),
        Line::from("  Open WhatsApp > Settings > Linked Devices > Link a Device"),
        Line::from("  Then scan the QR code displayed on screen."),
        Line::from(""),
        Line::from("  (Full QR rendering requires the terminal to support the QR code)"),
    ]
}
