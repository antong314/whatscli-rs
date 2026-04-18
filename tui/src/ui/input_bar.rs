use ratatui::{
    layout::Rect,
    style::{Color, Style},
    widgets::{Block, Borders, Paragraph},
    Frame,
};

use crate::state::{App, InputMode};

pub fn render(f: &mut Frame, area: Rect, app: &App) {
    let title = match app.input_mode {
        InputMode::Normal => " Input ",
        InputMode::Command => " Command ",
        InputMode::Search => " Search ",
    };

    let input = Paragraph::new(app.input_buffer.as_str())
        .block(
            Block::default()
                .borders(Borders::ALL)
                .title(title)
                .border_style(Style::default().fg(Color::Blue)),
        )
        .style(Style::default().fg(Color::White));

    f.render_widget(input, area);
}
