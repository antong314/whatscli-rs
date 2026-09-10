package server

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"time"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
	"github.com/antong314/whatscli-rs/backend/messages"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WhatsCLIServer implements the gRPC WhatsCLIServer interface.
type WhatsCLIServer struct {
	pb.UnimplementedWhatsCLIServer

	sm        *messages.SessionManager
	broadcast *Broadcaster
	handler   *GrpcHandler
}

func NewWhatsCLIServer(sm *messages.SessionManager, broadcast *Broadcaster, handler *GrpcHandler) *WhatsCLIServer {
	return &WhatsCLIServer{
		sm:        sm,
		broadcast: broadcast,
		handler:   handler,
	}
}

// EventStream handles the bidirectional event stream.
func (s *WhatsCLIServer) EventStream(stream pb.WhatsCLI_EventStreamServer) error {
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive handshake: %v", err)
	}
	handshake := first.GetHandshake()
	if handshake == nil {
		return status.Errorf(codes.InvalidArgument, "first message must be ConnectHandshake")
	}

	s.handler.SetViewportLines(int(handshake.ViewportRows))

	clientID, eventCh := s.broadcast.Subscribe()
	defer s.broadcast.Unsubscribe(clientID)

	// Send current state to the newly connected client.
	chats := s.sm.GetChats()
	pbChats := make([]*pb.ChatProto, 0, len(chats))
	for _, c := range chats {
		pbChats = append(pbChats, chatToProto(c))
	}
	if err := stream.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_ChatList{
			ChatList: &pb.ChatList{Chats: pbChats},
		},
	}); err != nil {
		return err
	}

	// Send current status.
	st := s.sm.GetStatus()
	if err := stream.Send(&pb.ServerEvent{
		Event: &pb.ServerEvent_StatusUpdate{
			StatusUpdate: &pb.StatusUpdate{
				BatteryCharge:    int32(st.BatteryCharge),
				BatteryLoading:   st.BatteryLoading,
				BatteryPowersave: st.BatteryPowersave,
				Connected:        st.Connected,
				LastSeen:         st.LastSeen,
			},
		},
	}); err != nil {
		return err
	}

	// Send events to the client in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		for event := range eventCh {
			if err := stream.Send(event); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	// Receive commands from the client.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		s.handleClientMessage(msg)

		select {
		case e := <-errCh:
			return e
		default:
		}
	}
}

// GetMedia streams the media bytes for a given message.
func (s *WhatsCLIServer) GetMedia(req *pb.MediaRequest, stream pb.WhatsCLI_GetMediaServer) error {
	started := time.Now()
	token := mediaLogToken(req.MessageId)
	fmt.Printf("[media-rpc] started message=%s\n", token)
	msg, ok := s.sm.GetMessageByID(req.MessageId)
	if !ok {
		fmt.Printf("[media-rpc] failed message=%s reason=not_found duration_ms=%d\n", token, time.Since(started).Milliseconds())
		return status.Errorf(codes.NotFound, "message not found: %s", req.MessageId)
	}

	path, err := s.sm.DownloadImage(msg)
	if err != nil {
		// WhatsApp's full-size media URLs expire. History still contains a
		// small JPEG preview, which is exactly what the shared-media gallery
		// needs. Returning it is much more useful than four broken tiles for
		// an otherwise valid historical message.
		if preview := embeddedMediaPreview(msg); len(preview) > 0 {
			fmt.Printf("[media-rpc] completed message=%s source=embedded_preview bytes=%d duration_ms=%d full_error=%v\n", token, len(preview), time.Since(started).Milliseconds(), err)
			return sendMediaBytes(preview, "image/jpeg", stream)
		}
		fmt.Printf("[media-rpc] failed message=%s reason=download duration_ms=%d error=%v\n", token, time.Since(started).Milliseconds(), err)
		return status.Errorf(codes.Internal, "failed to download media: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		fmt.Printf("[media-rpc] failed message=%s reason=open duration_ms=%d error=%v\n", token, time.Since(started).Milliseconds(), err)
		return status.Errorf(codes.Internal, "failed to open media file: %v", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		fmt.Printf("[media-rpc] failed message=%s reason=stat duration_ms=%d error=%v\n", token, time.Since(started).Milliseconds(), err)
		return status.Errorf(codes.Internal, "failed to stat media file: %v", err)
	}
	fmt.Printf("[media-rpc] streaming message=%s source=full bytes=%d download_ms=%d\n", token, info.Size(), time.Since(started).Milliseconds())

	buf := make([]byte, 64*1024)
	first := true
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := &pb.MediaChunk{
				Data: buf[:n],
			}
			if first {
				chunk.MimeType = msg.MimeType
				chunk.TotalSize = info.Size()
				first = false
			}
			if sendErr := stream.Send(chunk); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Internal, "error reading media: %v", err)
		}
	}
	fmt.Printf("[media-rpc] completed message=%s source=full bytes=%d duration_ms=%d\n", token, info.Size(), time.Since(started).Milliseconds())
	return nil
}

// embeddedMediaPreview extracts the preview WhatsApp stores alongside image,
// video, and document metadata. These bytes remain available after the
// full-size CDN URL expires.
func embeddedMediaPreview(msg messages.Message) []byte {
	if msg.RawMessage == nil {
		return nil
	}
	if media := msg.RawMessage.GetImageMessage(); media != nil {
		return media.GetJPEGThumbnail()
	}
	if media := msg.RawMessage.GetVideoMessage(); media != nil {
		return media.GetJPEGThumbnail()
	}
	if media := msg.RawMessage.GetDocumentMessage(); media != nil {
		return media.GetJPEGThumbnail()
	}
	return nil
}

func sendMediaBytes(data []byte, mimeType string, stream pb.WhatsCLI_GetMediaServer) error {
	const chunkSize = 64 * 1024
	for offset := 0; offset < len(data); offset += chunkSize {
		end := min(offset+chunkSize, len(data))
		chunk := &pb.MediaChunk{Data: data[offset:end]}
		if offset == 0 {
			chunk.MimeType = mimeType
			chunk.TotalSize = int64(len(data))
		}
		if err := stream.Send(chunk); err != nil {
			return err
		}
	}
	return nil
}

func mediaLogToken(messageID string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(messageID))
	return fmt.Sprintf("%016x", hash.Sum64())
}

// GetAvatar returns the profile picture for a chat. Unary — avatars are a
// few tens of KB at most. NOT_FOUND means the chat has no (visible) picture,
// which clients should treat as "fall back to initials", not as an error.
func (s *WhatsCLIServer) GetAvatar(ctx context.Context, req *pb.AvatarRequest) (*pb.AvatarResponse, error) {
	data, mimeType, err := s.sm.GetAvatar(req.ChatId, req.Preview)
	if err != nil {
		if errors.Is(err, messages.ErrNoAvatar) {
			return nil, status.Errorf(codes.NotFound, "no avatar for %s", req.ChatId)
		}
		return nil, status.Errorf(codes.Internal, "avatar fetch failed: %v", err)
	}
	return &pb.AvatarResponse{Data: data, MimeType: mimeType}, nil
}

// Login handles the QR code login flow.
func (s *WhatsCLIServer) Login(req *pb.LoginRequest, stream pb.WhatsCLI_LoginServer) error {
	loginCh := make(chan *pb.LoginEvent, 16)
	s.handler.SetLoginSink(loginCh)
	defer s.handler.SetLoginSink(nil)

	s.sm.CommandChannel <- messages.Command{Name: "login"}

	for evt := range loginCh {
		if err := stream.Send(evt); err != nil {
			return err
		}
		if evt.GetSuccess() != nil || evt.GetTimeout() != nil || evt.GetError() != nil {
			return nil
		}
	}
	return nil
}

// handleClientMessage dispatches a ClientMessage to the SessionManager's
// CommandChannel, translating typed protobuf variants back to the legacy
// Command{Name, Params} format.
func (s *WhatsCLIServer) handleClientMessage(msg *pb.ClientMessage) {
	var cmd messages.Command

	switch m := msg.Msg.(type) {
	case *pb.ClientMessage_Handshake:
		s.handler.SetViewportLines(int(m.Handshake.ViewportRows))
		return

	case *pb.ClientMessage_SelectChat:
		intent := messages.SelectIntentProbe
		if m.SelectChat.GetIntent() == pb.SelectChat_COMMIT {
			intent = messages.SelectIntentCommit
		}
		cmd = messages.Command{
			Name:   "select",
			Params: []string{m.SelectChat.ChatId},
			Intent: intent,
		}
	case *pb.ClientMessage_RequestBacklog:
		cmd = messages.Command{Name: "backlog"}

	case *pb.ClientMessage_SendText:
		cmd = messages.Command{Name: "send", Params: []string{m.SendText.ChatId, m.SendText.Text}}
	case *pb.ClientMessage_SendMedia:
		cmd = messages.Command{Name: commandNameForMediaKind(m.SendMedia.Kind), Params: []string{m.SendMedia.FilePath}}

	case *pb.ClientMessage_MarkRead:
		cmd = messages.Command{Name: "read"}
	case *pb.ClientMessage_MarkUnread:
		cmd = messages.Command{Name: "unread"}

	case *pb.ClientMessage_Login:
		cmd = messages.Command{Name: "login"}
	case *pb.ClientMessage_Disconnect:
		cmd = messages.Command{Name: "disconnect"}
	case *pb.ClientMessage_Logout:
		cmd = messages.Command{Name: "logout"}

	case *pb.ClientMessage_DownloadMedia:
		cmd = messages.Command{Name: "download", Params: []string{m.DownloadMedia.MessageId}}
	case *pb.ClientMessage_OpenMedia:
		cmd = messages.Command{Name: "open", Params: []string{m.OpenMedia.MessageId}}
	case *pb.ClientMessage_ShowMedia:
		cmd = messages.Command{Name: "show", Params: []string{m.ShowMedia.MessageId}}

	case *pb.ClientMessage_Revoke:
		cmd = messages.Command{Name: "revoke", Params: []string{m.Revoke.MessageId}}
	case *pb.ClientMessage_ForceTranslate:
		cmd = messages.Command{Name: "forcetranslate", Params: []string{m.ForceTranslate.MessageId}}
	case *pb.ClientMessage_LeaveGroup:
		cmd = messages.Command{Name: "leave"}
	case *pb.ClientMessage_CreateGroup:
		params := []string{m.CreateGroup.Subject}
		params = append(params, m.CreateGroup.Participants...)
		cmd = messages.Command{Name: "create", Params: params}
	case *pb.ClientMessage_GroupMember:
		action := "add"
		switch m.GroupMember.Action {
		case pb.GroupMember_ADD:
			action = "add"
		case pb.GroupMember_REMOVE:
			action = "remove"
		case pb.GroupMember_PROMOTE:
			action = "admin"
		case pb.GroupMember_DEMOTE:
			action = "removeadmin"
		}
		cmd = messages.Command{Name: action, Params: m.GroupMember.Participants}
	case *pb.ClientMessage_SetSubject:
		cmd = messages.Command{Name: "subject", Params: []string{m.SetSubject.Subject}}

	case *pb.ClientMessage_GetInfo:
		cmd = messages.Command{Name: "info", Params: []string{m.GetInfo.MessageId}}
	case *pb.ClientMessage_GetUrl:
		cmd = messages.Command{Name: "url", Params: []string{m.GetUrl.MessageId}}

	case *pb.ClientMessage_SendImage:
		cmd = messages.Command{Name: "sendimage", Params: []string{m.SendImage.FilePath}}
	case *pb.ClientMessage_SendVideo:
		cmd = messages.Command{Name: "sendvideo", Params: []string{m.SendVideo.FilePath}}
	case *pb.ClientMessage_SendAudio:
		cmd = messages.Command{Name: "sendaudio", Params: []string{m.SendAudio.FilePath}}

	default:
		fmt.Printf("unhandled client message type: %T\n", msg.Msg)
		return
	}

	s.sm.CommandChannel <- cmd
}

// commandNameForMediaKind preserves the structured client's media type when
// bridging into the backend's legacy Command API. Falling back to upload keeps
// unknown/future values safe as documents instead of mislabelling their bytes.
func commandNameForMediaKind(kind pb.MessageKind) string {
	switch kind {
	case pb.MessageKind_IMAGE:
		return "sendimage"
	case pb.MessageKind_VIDEO:
		return "sendvideo"
	case pb.MessageKind_AUDIO:
		return "sendaudio"
	default:
		return "upload"
	}
}
