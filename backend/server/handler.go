package server

import (
	"io"
	"log"
	"sync"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
	"github.com/antong314/whatscli-rs/backend/messages"
	"github.com/skratchdot/open-golang/open"
)

// GrpcHandler implements messages.UiMessageHandler by forwarding all calls to
// the Broadcaster as ServerEvent messages.
type GrpcHandler struct {
	broadcast     *Broadcaster
	viewportLines int
	viewportMu    sync.RWMutex

	// loginSink receives QR events during the login flow. When non-nil, QR
	// data is written here instead of being broadcast.
	loginSink   chan *pb.LoginEvent
	loginSinkMu sync.Mutex
}

func NewGrpcHandler(b *Broadcaster) *GrpcHandler {
	return &GrpcHandler{
		broadcast:     b,
		viewportLines: 40,
	}
}

func (h *GrpcHandler) SetViewportLines(lines int) {
	h.viewportMu.Lock()
	defer h.viewportMu.Unlock()
	if lines > 0 {
		h.viewportLines = lines
	}
}

func (h *GrpcHandler) SetLoginSink(ch chan *pb.LoginEvent) {
	h.loginSinkMu.Lock()
	defer h.loginSinkMu.Unlock()
	h.loginSink = ch
}

// --- UiMessageHandler implementation ---

func (h *GrpcHandler) NewMessage(msg messages.Message) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_NewMessage{
			NewMessage: &pb.NewMessage{
				Message: messageToProto(msg),
			},
		},
	})
}

func (h *GrpcHandler) NewTranslation(msg messages.Message, text string) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_NewTranslation{
			NewTranslation: &pb.NewTranslation{
				MessageId:      msg.Id,
				TranslatedText: text,
			},
		},
	})
}

func (h *GrpcHandler) NewTranscription(msg messages.Message, text string) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_NewTranscription{
			NewTranscription: &pb.NewTranscription{
				MessageId:       msg.Id,
				TranscribedText: text,
			},
		},
	})
}

func (h *GrpcHandler) NewScreen(msgs []messages.Message) {
	pbMsgs := make([]*pb.MessageProto, 0, len(msgs))
	var chatID string
	for _, m := range msgs {
		pbMsgs = append(pbMsgs, messageToProto(m))
		if chatID == "" && m.ChatId != "" {
			chatID = m.ChatId
		}
	}
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_ChatMessages{
			ChatMessages: &pb.ChatMessages{
				ChatId:   chatID,
				Messages: pbMsgs,
			},
		},
	})
}

func (h *GrpcHandler) MessageStatus(chatID string, messageIDs []string, status messages.MessageStatus) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_MessageStatus{
			MessageStatus: &pb.MessageStatusUpdate{
				ChatId:     chatID,
				MessageIds: messageIDs,
				Status:     messageStatusToProto(status),
			},
		},
	})
}

func (h *GrpcHandler) SetChats(chats []messages.Chat) {
	pbChats := make([]*pb.ChatProto, 0, len(chats))
	for _, c := range chats {
		pbChats = append(pbChats, chatToProto(c))
	}
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_ChatList{
			ChatList: &pb.ChatList{
				Chats: pbChats,
			},
		},
	})
}

func (h *GrpcHandler) PrintError(err error) {
	if err == nil {
		return
	}
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_ErrorEvent{
			ErrorEvent: &pb.ErrorEvent{
				Text: err.Error(),
			},
		},
	})
}

func (h *GrpcHandler) PrintText(msg string) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_InfoText{
			InfoText: &pb.InfoText{
				Text: msg,
			},
		},
	})
}

func (h *GrpcHandler) PrintFile(path string) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_FileReady{
			FileReady: &pb.FileReady{
				FilePath: path,
			},
		},
	})
}

// FileSaved emits a FileReady event with the originating message id, so
// the TUI can pin the "→ saved to …" annotation under the right message
// in the chat view. Used for explicit downloads triggered by the user
// (s/o keys, /download, /open, /show).
func (h *GrpcHandler) FileSaved(messageID, path string) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_FileReady{
			FileReady: &pb.FileReady{
				MessageId: messageID,
				FilePath:  path,
			},
		},
	})
}

func (h *GrpcHandler) SetStatus(status messages.SessionStatus) {
	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_StatusUpdate{
			StatusUpdate: &pb.StatusUpdate{
				BatteryCharge:    int32(status.BatteryCharge),
				BatteryLoading:   status.BatteryLoading,
				BatteryPowersave: status.BatteryPowersave,
				Connected:        status.Connected,
				LastSeen:         status.LastSeen,
			},
		},
	})
}

// OpenFile asks the host OS to open `path` with whatever the user has
// configured as the default app for that file type (Preview for PDFs,
// QuickTime for movies, the browser for URLs, etc.) and *also* tells the
// TUI we did so via an OpenFileRequest event so the front-end can show a
// confirmation toast.
//
// The OS-side open is fired in a goroutine: `open.Run` shells out to
// `open` / `xdg-open` which can block briefly while LaunchServices spins
// up the app, and we don't want that to stall the gRPC event stream.
//
// Note: this works because backend and TUI run on the same machine in
// our split architecture. If we ever ship a remote backend we'll need
// to push the open responsibility to the TUI side instead — at that
// point the OpenFileRequest event below becomes the *only* trigger and
// this goroutine should be removed.
func (h *GrpcHandler) OpenFile(path string) {
	if path != "" {
		go func(p string) {
			if err := open.Run(p); err != nil {
				log.Printf("OpenFile: failed to open %q: %v", p, err)
			}
		}(path)
	}

	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_OpenFile{
			OpenFile: &pb.OpenFileRequest{
				FilePath: path,
			},
		},
	})
}

// GetWriter returns an io.Writer. In headless mode this is a no-op sink
// since QR codes now go through the QRCode method instead.
func (h *GrpcHandler) GetWriter() io.Writer {
	return io.Discard
}

func (h *GrpcHandler) GetViewportLines() int {
	h.viewportMu.RLock()
	defer h.viewportMu.RUnlock()
	return h.viewportLines
}

func (h *GrpcHandler) QRCode(code string) {
	h.loginSinkMu.Lock()
	sink := h.loginSink
	h.loginSinkMu.Unlock()

	if sink != nil {
		sink <- &pb.LoginEvent{
			Event: &pb.LoginEvent_QrCode{
				QrCode: &pb.QrCode{
					Text: code,
				},
			},
		}
	}
}

func (h *GrpcHandler) QREvent(event string) {
	h.loginSinkMu.Lock()
	sink := h.loginSink
	h.loginSinkMu.Unlock()

	if sink != nil {
		if event == "timeout" {
			sink <- &pb.LoginEvent{
				Event: &pb.LoginEvent_Timeout{
					Timeout: &pb.LoginTimeout{},
				},
			}
			close(sink)
		}
	} else {
		h.broadcast.Send(&pb.ServerEvent{
			Event: &pb.ServerEvent_InfoText{
				InfoText: &pb.InfoText{
					Text: "QR event: " + event,
				},
			},
		})
	}
}

func (h *GrpcHandler) LoginSuccess() {
	h.loginSinkMu.Lock()
	sink := h.loginSink
	h.loginSinkMu.Unlock()

	if sink != nil {
		sink <- &pb.LoginEvent{
			Event: &pb.LoginEvent_Success{
				Success: &pb.LoginSuccess{},
			},
		}
		close(sink)
	}

	h.broadcast.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_InfoText{
			InfoText: &pb.InfoText{
				Text: "Successfully logged in!",
			},
		},
	})
}

// --- Type conversion helpers ---

func messageToProto(m messages.Message) *pb.MessageProto {
	return &pb.MessageProto{
		Id:           m.Id,
		ChatId:       m.ChatId,
		SenderId:     m.SenderId,
		ContactId:    m.ContactId,
		ContactName:  m.ContactName,
		ContactShort: m.ContactShort,
		Timestamp:    m.Timestamp,
		FromMe:       m.FromMe,
		Forwarded:    m.Forwarded,
		Text:         m.Text,
		Kind:         messageKindToProto(m.Kind),
		MimeType:     m.MimeType,
		FileName:     m.FileName,
		Unread:       m.Unread,
		Status:       messageStatusToProto(m.Status),
	}
}

func messageStatusToProto(s messages.MessageStatus) pb.MessageStatus {
	switch s {
	case messages.MessageStatusPending:
		return pb.MessageStatus_MESSAGE_STATUS_PENDING
	case messages.MessageStatusSent:
		return pb.MessageStatus_MESSAGE_STATUS_SENT
	case messages.MessageStatusDelivered:
		return pb.MessageStatus_MESSAGE_STATUS_DELIVERED
	case messages.MessageStatusRead:
		return pb.MessageStatus_MESSAGE_STATUS_READ
	default:
		return pb.MessageStatus_MESSAGE_STATUS_UNKNOWN
	}
}

func messageKindToProto(k messages.MessageKind) pb.MessageKind {
	switch k {
	case messages.MessageKindText:
		return pb.MessageKind_TEXT
	case messages.MessageKindImage:
		return pb.MessageKind_IMAGE
	case messages.MessageKindVideo:
		return pb.MessageKind_VIDEO
	case messages.MessageKindAudio:
		return pb.MessageKind_AUDIO
	case messages.MessageKindDocument:
		return pb.MessageKind_DOCUMENT
	default:
		return pb.MessageKind_UNKNOWN
	}
}

func chatToProto(c messages.Chat) *pb.ChatProto {
	return &pb.ChatProto{
		Id:          c.Id,
		IsGroup:     c.IsGroup,
		Name:        c.Name,
		Unread:      int32(c.Unread),
		LastMessage: c.LastMessage,
		Archived:    c.Archived,
		Pinned:      c.Pinned,
	}
}

