// this package manages the messages
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
	// FileSaved is the structured "save complete" hook: it carries the
	// message id alongside the on-disk path so the gRPC layer can emit a
	// FileReady event with both fields. The legacy tview UI uses
	// PrintFile (path-only); the gRPC server uses FileSaved so the Rust
	// TUI can correlate the saved file back to the originating message
	// (and render the inline "→ saved to …" annotation under it).
	//
	// Both are called for the same event in the gRPC handler so logs /
	// info text continue to work for any consumer that only watches
	// PrintFile.
	FileSaved(messageID, path string)
	// MessageStatus reports delivery/read upgrades for our own messages
	// (WhatsApp tick marks). The legacy tview UI ignores it.
	MessageStatus(chatID string, messageIDs []string, status MessageStatus)
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

// SelectIntent describes how strongly the client believes the user wants
// the chat referenced by a "select" Command. PROBE selections (e.g. arrow-key
// navigation through the chat list) defer the auto-mark-as-read read receipt;
// COMMIT selections (e.g. Enter on a chat row) fire it immediately.
//
// Defaulting to PROBE means any caller that forgets to set the intent gets
// the safer behaviour - we won't accidentally clear unread state for a chat
// the user only briefly arrowed past.
type SelectIntent int

const (
	SelectIntentProbe  SelectIntent = 0
	SelectIntentCommit SelectIntent = 1
)

// message object for commands
type Command struct {
	Name   string
	Params []string
	// Intent is meaningful only for `Name == "select"`; it is otherwise zero
	// (PROBE) and ignored. Adding it as a struct field instead of a magic
	// string in Params keeps the wire-format/dispatch code obvious.
	Intent SelectIntent
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

// MessageStatus is the send/delivery state of an OUTGOING message — the
// WhatsApp tick marks. Empty for incoming messages. States only ever
// upgrade (pending → sent → delivered → read); receipts can arrive out of
// order, so use statusRank before overwriting.
type MessageStatus string

const (
	MessageStatusPending   MessageStatus = "pending"   // clock
	MessageStatusSent      MessageStatus = "sent"      // one grey check
	MessageStatusDelivered MessageStatus = "delivered" // two grey checks
	MessageStatusRead      MessageStatus = "read"      // two blue checks
)

func statusRank(s MessageStatus) int {
	switch s {
	case MessageStatusPending:
		return 1
	case MessageStatusSent:
		return 2
	case MessageStatusDelivered:
		return 3
	case MessageStatusRead:
		return 4
	default:
		return 0
	}
}

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
	Status       MessageStatus
	Reactions    []MessageReaction
	RawMessage   *waProto.Message
}

// MessageReaction is one participant's current reaction to a message.
// WhatsApp sends an empty emoji to remove that participant's reaction, so
// only non-empty values are stored here.
type MessageReaction struct {
	SenderId string `json:"sender_id"`
	Emoji    string `json:"emoji"`
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
