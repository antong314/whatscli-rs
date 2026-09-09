package server

import (
	"testing"

	pb "github.com/antong314/whatscli-rs/backend/gen/pb"
	"github.com/antong314/whatscli-rs/backend/messages"
)

func TestCommandNameForMediaKind(t *testing.T) {
	tests := []struct {
		kind pb.MessageKind
		want string
	}{
		{pb.MessageKind_IMAGE, "sendimage"},
		{pb.MessageKind_VIDEO, "sendvideo"},
		{pb.MessageKind_AUDIO, "sendaudio"},
		{pb.MessageKind_DOCUMENT, "upload"},
		{pb.MessageKind_UNKNOWN, "upload"},
	}
	for _, test := range tests {
		if got := commandNameForMediaKind(test.kind); got != test.want {
			t.Errorf("commandNameForMediaKind(%v) = %q, want %q", test.kind, got, test.want)
		}
	}
}

func TestHandleClientMessagePreservesImageKind(t *testing.T) {
	sm := &messages.SessionManager{CommandChannel: make(chan messages.Command, 1)}
	server := &WhatsCLIServer{sm: sm}
	server.handleClientMessage(&pb.ClientMessage{
		Msg: &pb.ClientMessage_SendMedia{SendMedia: &pb.SendMedia{
			ChatId:   "chat@s.whatsapp.net",
			FilePath: "/tmp/pasted.png",
			Kind:     pb.MessageKind_IMAGE,
		}},
	})

	command := <-sm.CommandChannel
	if command.Name != "sendimage" {
		t.Fatalf("pasted image became %q command, want sendimage", command.Name)
	}
	if len(command.Params) != 1 || command.Params[0] != "/tmp/pasted.png" {
		t.Fatalf("unexpected image parameters: %#v", command.Params)
	}
}
