package server

import (
	"io"
	"sync"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
	"github.com/antong314/whatscli-rs/backend/messages"
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

func (h *GrpcHandler) OpenFile(path string) {
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

