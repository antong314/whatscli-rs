use std::path::PathBuf;
use hyper_util::rt::TokioIo;
use tokio::sync::mpsc;
use tonic::transport::{Endpoint, Uri};
use tower::service_fn;

use crate::proto::whatscli::{
    self as pb,
    whats_cli_client::WhatsCliClient,
    ClientMessage, ConnectHandshake, ServerEvent,
};

pub struct ClientHandle {
    tx: mpsc::UnboundedSender<ClientMessage>,
}

impl ClientHandle {
    pub fn send(&self, msg: ClientMessage) {
        let _ = self.tx.send(msg);
    }
}

pub async fn connect(
    socket_path: PathBuf,
) -> color_eyre::Result<(ClientHandle, mpsc::Receiver<ServerEvent>)> {
    let path = socket_path.clone();
    let channel = Endpoint::try_from("http://[::]:50051")?
        .connect_with_connector(service_fn(move |_: Uri| {
            let path = path.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(path).await?;
                Ok::<_, std::io::Error>(TokioIo::new(stream))
            }
        }))
        .await?;

    let mut client = WhatsCliClient::new(channel);

    let (cmd_tx, mut cmd_rx) = mpsc::unbounded_channel::<ClientMessage>();
    let (event_tx, event_rx) = mpsc::channel::<ServerEvent>(256);

    let outgoing = async_stream::stream! {
        yield ClientMessage {
            msg: Some(pb::client_message::Msg::Handshake(ConnectHandshake {
                viewport_rows: 40,
                viewport_cols: 120,
                client_version: env!("CARGO_PKG_VERSION").to_string(),
            })),
        };

        while let Some(msg) = cmd_rx.recv().await {
            yield msg;
        }
    };

    let response = client.event_stream(outgoing).await?;
    let mut stream = response.into_inner();

    tokio::spawn(async move {
        loop {
            match stream.message().await {
                Ok(Some(event)) => {
                    if event_tx.send(event).await.is_err() {
                        break;
                    }
                }
                Ok(None) => break,
                Err(_) => break,
            }
        }
    });

    Ok((ClientHandle { tx: cmd_tx }, event_rx))
}
