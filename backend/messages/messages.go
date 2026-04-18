//this package manages the messages
package messages

import (
	"io"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// TODO: move these funcs/interface to channels
type UiMessageHandler interface {
	NewMessage(Message)
	NewTranslation(Message, string)
	NewTranscription(Message, string)
	NewScreen([]Message)
	SetChats([]Chat)
	PrintError(error)
	PrintText(string)
	PrintFile(string)
	SetStatus(SessionStatus)
	OpenFile(string)
	GetWriter() io.Writer
	GetViewportLines() int
	// QRCode is called with the raw QR code string during login. Handlers can
	// render it however they like (ANSI in terminal, image via gRPC, etc.).
	QRCode(code string)
	// QREvent is called with other QR channel events (e.g. "timeout").
	QREvent(event string)
	// LoginSuccess is called when QR pairing succeeds.
	LoginSuccess()
}

// data struct for current session status
type SessionStatus struct {
	BatteryCharge    int
	BatteryLoading   bool
	BatteryPowersave bool
	Connected        bool
	LastSeen         string
}

// message struct for battery messages
type BatteryMsg struct {
	charge    int
	loading   bool
	powersave bool
}

// message struct for status messages
type StatusMsg struct {
	connected bool
	err       error
}

// message object for commands
type Command struct {
	Name   string
	Params []string
}

type MessageKind string

const (
	MessageKindText     MessageKind = "text"
	MessageKindImage    MessageKind = "image"
	MessageKindVideo    MessageKind = "video"
	MessageKindAudio    MessageKind = "audio"
	MessageKindDocument MessageKind = "document"
	MessageKindUnknown  MessageKind = "unknown"
)

// internal message representation to abstract from message lib
type Message struct {
	Id           string
	ChatId       string // the source of the message (group id or contact id)
	SenderId     string
	ContactId    string
	ContactName  string
	ContactShort string
	Timestamp    uint64
	FromMe       bool
	Forwarded    bool
	Text         string
	Kind         MessageKind
	MimeType     string
	FileName     string
	Unread       bool
	RawMessage   *waProto.Message
}

// internal contact representation to abstract from message lib
type Chat struct {
	Id          string
	IsGroup     bool
	Name        string
	Unread      int
	LastMessage int64
	Archived    bool
	Pinned      bool
}

type Contact struct {
	Id    string
	Name  string
	Short string
}

const GROUPSUFFIX = "@g.us"
const CONTACTSUFFIX = "@s.whatsapp.net"
const STATUSSUFFIX = "status@broadcast"
