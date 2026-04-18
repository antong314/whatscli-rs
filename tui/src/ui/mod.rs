pub mod chat_list;
pub mod input_bar;
pub mod message_view;
pub mod qr_login;
pub mod status_bar;

use ratatui::{
    layout::{Constraint, Direction, Layout},
    Frame,
};

use crate::state::App;

pub fn draw(f: &mut Frame, app: &mut App) {
    let outer = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(1),
            Constraint::Min(5),
            Constraint::Length(3),
        ])
        .split(f.area());

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
}
