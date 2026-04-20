use ratatui::{
    layout::Rect,
    style::{Color, Style},
    widgets::{Block, Borders, Paragraph},
    Frame,
};

use crate::state::{App, InputMode};

pub fn render(f: &mut Frame, area: Rect, app: &App) {
    match app.input_mode {
        InputMode::Normal => {
            // The composer is a tui-textarea TextArea that already owns its own
            // block, styling, and cursor.
            f.render_widget(&app.composer, area);
        }
        InputMode::Command | InputMode::Search => render_modal_input(f, area, app),
    }
}

fn render_modal_input(f: &mut Frame, area: Rect, app: &App) {
    let title = match app.input_mode {
        InputMode::Command => " Command ",
        InputMode::Search => " Search ",
        InputMode::Normal => unreachable!(),
    };

    let input = Paragraph::new(app.input_buffer.as_str())
        .block(
            Block::default()
                .borders(Borders::ALL)
                .title(title)
                .border_style(Style::default().fg(Color::Yellow)),
        )
        .style(Style::default().fg(Color::White));

    f.render_widget(input, area);

    // Position the terminal cursor at the end of the modal input. Add 1 to
    // account for the left border.
    let cursor_x = area.x + 1 + app.input_buffer.chars().count() as u16;
    let cursor_y = area.y + 1;
    if cursor_x < area.x + area.width.saturating_sub(1) {
        f.set_cursor_position((cursor_x, cursor_y));
    }
}
