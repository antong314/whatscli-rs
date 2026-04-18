use std::path::PathBuf;

pub struct Config {
    pub socket_path: PathBuf,
    pub chat_sidebar_width: u16,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            socket_path: default_socket_path(),
            chat_sidebar_width: 30,
        }
    }
}

fn default_socket_path() -> PathBuf {
    if let Ok(xdg) = std::env::var("XDG_RUNTIME_DIR") {
        return PathBuf::from(xdg).join("whatscli").join("whatscli.sock");
    }
    std::env::temp_dir().join("whatscli").join("whatscli.sock")
}
