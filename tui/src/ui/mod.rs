pub mod chat_list;
pub mod help;
pub mod input_bar;
pub mod message_view;
pub mod qr_login;
pub mod status_bar;
pub mod timestamps;
pub mod wrap;

use ratatui::{
    layout::{Constraint, Direction, Layout},
    Frame,
};

use crate::state::{App, InputMode, COMPOSER_MAX_ROWS, COMPOSER_MIN_ROWS};

pub fn draw(f: &mut Frame, app: &mut App) {
    let total_area = f.area();
    let input_height = compute_input_height(app, total_area.width);

    let outer = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(1),
            Constraint::Min(5),
            Constraint::Length(input_height),
        ])
        .split(total_area);

    status_bar::render(f, outer[0], app);

    if let Some(ref qr) = app.qr_code.clone() {
        qr_login::render(f, outer[1], qr);
    } else {
        let main_area = Layout::default()
            .direction(Direction::Horizontal)
            .constraints([
                Constraint::Percentage(30),
                Constraint::Percentage(70),
            ])
            .split(outer[1]);

        chat_list::render(f, main_area[0], app);
        message_view::render(f, main_area[1], app);
    }

    input_bar::render(f, outer[2], app);

    // Modal layer: drawn last so it overlays everything beneath it.
    if app.show_help {
        let max_scroll = help::render_modal(f, total_area, app);
        // Clamp scroll to the actual content height so a user who paged past
        // the end on a tall window doesn't see a blank popup if they later
        // resize down (or vice versa).
        if app.help_scroll > max_scroll {
            app.help_scroll = max_scroll;
        }
    }
}

/// Pick the input area height for this frame.
///
/// Modal Command/Search bars are always single-line (3 rows including borders).
/// The composer auto-grows from `COMPOSER_MIN_ROWS` to `COMPOSER_MAX_ROWS` based
/// on its content for the available width.
fn compute_input_height(app: &mut App, width: u16) -> u16 {
    match app.input_mode {
        InputMode::Command | InputMode::Search => 3,
        InputMode::Normal => {
            let m = app.composer.measure(width);
            m.preferred_rows.clamp(COMPOSER_MIN_ROWS, COMPOSER_MAX_ROWS)
        }
    }
}
