use std::collections::HashMap;
use std::io::Cursor;

use image::DynamicImage;
use ratatui_image::picker::Picker;
use ratatui_image::protocol::StatefulProtocol;
use tokio::sync::mpsc;

use crate::proto::whatscli as pb;

pub struct ImageCache {
    cache: HashMap<String, StatefulProtocol>,
    dims: HashMap<String, (u32, u32)>,
    loading: std::collections::HashSet<String>,
    failed: std::collections::HashSet<String>,
    picker: Option<Picker>,
}

impl ImageCache {
    pub fn new() -> Self {
        Self {
            cache: HashMap::new(),
            dims: HashMap::new(),
            loading: std::collections::HashSet::new(),
            failed: std::collections::HashSet::new(),
            picker: None,
        }
    }

    pub fn init_picker(&mut self) {
        if self.picker.is_some() {
            return;
        }
        let in_tmux = std::env::var("TMUX").is_ok()
            || std::env::var("TERM_PROGRAM").as_deref() == Ok("tmux");
        if in_tmux {
            #[allow(deprecated)]
            {
                self.picker = Some(Picker::from_fontsize((7, 14)));
            }
        } else {
            self.picker = Picker::from_query_stdio().ok();
        }
    }

    pub fn clear(&mut self) {
        self.cache.clear();
        self.dims.clear();
        self.loading.clear();
        self.failed.clear();
    }

    pub fn dimensions(&self, message_id: &str) -> Option<(u32, u32)> {
        self.dims.get(message_id).copied()
    }

    pub fn font_size(&self) -> Option<(u16, u16)> {
        self.picker.as_ref().map(|p| p.font_size())
    }

    pub fn get_mut(&mut self, message_id: &str) -> Option<&mut StatefulProtocol> {
        self.cache.get_mut(message_id)
    }

    pub fn contains(&self, message_id: &str) -> bool {
        self.cache.contains_key(message_id)
    }

    pub fn is_loading(&self, message_id: &str) -> bool {
        self.loading.contains(message_id)
    }

    pub fn mark_loading(&mut self, message_id: &str) {
        self.loading.insert(message_id.to_string());
    }

    pub fn is_failed(&self, message_id: &str) -> bool {
        self.failed.contains(message_id)
    }

    pub fn handle_response(&mut self, message_id: String, img: Option<DynamicImage>) {
        self.loading.remove(&message_id);
        let Some(img) = img else {
            self.failed.insert(message_id);
            return;
        };
        if let Some(picker) = &self.picker {
            let dims = (img.width(), img.height());
            let protocol = picker.new_resize_protocol(img);
            self.cache.insert(message_id.clone(), protocol);
            self.dims.insert(message_id, dims);

            if self.cache.len() > 50 {
                if let Some(oldest) = self.cache.keys().next().cloned() {
                    self.cache.remove(&oldest);
                    self.dims.remove(&oldest);
                }
            }
        }
    }

    pub fn loading_count(&self) -> usize {
        self.loading.len()
    }

    pub fn has_graphics_support(&self) -> bool {
        self.picker.is_some()
    }
}

pub struct ImageLoadResponse {
    pub message_id: String,
    pub image: Option<DynamicImage>,
}

pub fn spawn_media_fetcher(
    socket_path: std::path::PathBuf,
) -> (
    mpsc::UnboundedSender<String>,
    mpsc::Receiver<ImageLoadResponse>,
) {
    let (request_tx, mut request_rx) = mpsc::unbounded_channel::<String>();
    let (response_tx, response_rx) = mpsc::channel::<ImageLoadResponse>(32);

    let semaphore = std::sync::Arc::new(tokio::sync::Semaphore::new(3));

    tokio::spawn(async move {
        while let Some(message_id) = request_rx.recv().await {
            let path = socket_path.clone();
            let tx = response_tx.clone();
            let mid = message_id.clone();
            let sem = semaphore.clone();

            tokio::spawn(async move {
                let _permit = match sem.acquire().await {
                    Ok(p) => p,
                    Err(_) => return,
                };
                let result = fetch_and_decode(path, &mid).await.ok();
                let _ = tx
                    .send(ImageLoadResponse {
                        message_id: mid,
                        image: result,
                    })
                    .await;
            });
        }
    });

    (request_tx, response_rx)
}

async fn fetch_and_decode(
    socket_path: std::path::PathBuf,
    message_id: &str,
) -> color_eyre::Result<DynamicImage> {
    use hyper_util::rt::TokioIo;
    use tonic::transport::{Endpoint, Uri};
    use tower::service_fn;

    let path = socket_path.clone();
    let channel = Endpoint::try_from("http://[::]:50051")?
        .connect_with_connector(service_fn(move |_: Uri| {
            let p = path.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(p).await?;
                Ok::<_, std::io::Error>(TokioIo::new(stream))
            }
        }))
        .await?;

    let mut client = pb::whats_cli_client::WhatsCliClient::new(channel);

    let request = pb::MediaRequest {
        message_id: message_id.to_string(),
    };

    let response = client.get_media(request).await?;
    let mut stream = response.into_inner();

    let mut data = Vec::new();
    while let Some(chunk) = stream.message().await? {
        data.extend_from_slice(&chunk.data);
    }

    let reader = Cursor::new(data);
    let img = image::ImageReader::new(reader)
        .with_guessed_format()?
        .decode()?;

    Ok(img)
}
