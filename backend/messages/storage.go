package messages

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antong314/whatscli-rs/backend/config"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

// MessageDatabase stores messages and contact data.
type MessageDatabase struct {
	messages     map[string][]Message
	messagesById map[string]Message
	chats        map[string]Chat
	contacts     map[string]Contact
	translations    map[string]string
	transcriptions  map[string]string

	contactLock      sync.RWMutex
	chatLock         sync.RWMutex
	messageLock      sync.RWMutex
	translationLock  sync.RWMutex
	transcriptionLock sync.RWMutex
}

// Init initializes the message database.
func (md *MessageDatabase) Init() {
	md.messages = make(map[string][]Message)
	md.messagesById = make(map[string]Message)
	md.chats = make(map[string]Chat)
	md.contacts = make(map[string]Contact)
	md.translations = make(map[string]string)
	md.transcriptions = make(map[string]string)
}

// AddMessage stores a message and updates related chat state.
func (md *MessageDatabase) AddMessage(msg Message, markUnread bool) bool {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	if existing, ok := md.messagesById[msg.Id]; ok {
		// Keep the first version, but upgrade metadata if the newer message has richer data.
		if existing.RawMessage == nil && msg.RawMessage != nil {
			existing.RawMessage = msg.RawMessage
		}
		if existing.Kind == MessageKindUnknown && msg.Kind != MessageKindUnknown {
			existing.Kind = msg.Kind
		}
		if existing.Text == "" && msg.Text != "" {
			existing.Text = msg.Text
		}
		if existing.FileName == "" && msg.FileName != "" {
			existing.FileName = msg.FileName
		}
		if existing.MimeType == "" && msg.MimeType != "" {
			existing.MimeType = msg.MimeType
		}
		if statusRank(msg.Status) > statusRank(existing.Status) {
			existing.Status = msg.Status
		}
		// Only bump the chat counter on a false→true transition: re-delivered
		// messages (offline sync, cache reload) must not count twice.
		newlyUnread := markUnread && !existing.Unread
		existing.Unread = existing.Unread || markUnread
		md.messagesById[msg.Id] = existing
		md.replaceMessageLocked(existing)
		md.updateChatFromMessageLocked(existing, newlyUnread)
		return false
	}

	msg.Unread = markUnread
	md.messagesById[msg.Id] = msg
	md.messages[msg.ChatId] = append(md.messages[msg.ChatId], msg)
	sort.Slice(md.messages[msg.ChatId], func(i, j int) bool {
		if md.messages[msg.ChatId][i].Timestamp == md.messages[msg.ChatId][j].Timestamp {
			return md.messages[msg.ChatId][i].Id < md.messages[msg.ChatId][j].Id
		}
		return md.messages[msg.ChatId][i].Timestamp < md.messages[msg.ChatId][j].Timestamp
	})
	md.updateChatFromMessageLocked(msg, markUnread)
	return true
}

func (md *MessageDatabase) replaceMessageLocked(msg Message) {
	msgs := md.messages[msg.ChatId]
	for idx, current := range msgs {
		if current.Id == msg.Id {
			msgs[idx] = msg
			md.messages[msg.ChatId] = msgs
			return
		}
	}
}

func (md *MessageDatabase) updateChatFromMessageLocked(msg Message, markUnread bool) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()

	chat, exists := md.chats[msg.ChatId]
	if !exists {
		chat = Chat{
			Id:      msg.ChatId,
			IsGroup: strings.Contains(msg.ChatId, GROUPSUFFIX),
			Name:    msg.ContactName,
		}
	}
	if chat.Name == "" {
		chat.Name = msg.ContactName
	}
	if int64(msg.Timestamp) > chat.LastMessage {
		chat.LastMessage = int64(msg.Timestamp)
	}
	if markUnread {
		chat.Unread++
	}
	md.chats[msg.ChatId] = chat

	if msg.ContactId != "" {
		md.contactLock.Lock()
		if _, ok := md.contacts[msg.ContactId]; !ok {
			md.contacts[msg.ContactId] = Contact{
				Id:    msg.ContactId,
				Name:  msg.ContactName,
				Short: msg.ContactShort,
			}
		}
		md.contactLock.Unlock()
	}
}

// AddChat adds or updates a chat in the database.
func (md *MessageDatabase) AddChat(chat Chat) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()

	existing, ok := md.chats[chat.Id]
	if ok {
		if chat.Name == "" {
			chat.Name = existing.Name
		}
		if chat.LastMessage < existing.LastMessage {
			chat.LastMessage = existing.LastMessage
		}
		if chat.Unread < existing.Unread {
			chat.Unread = existing.Unread
		}
		if !chat.Archived && existing.Archived {
			chat.Archived = existing.Archived
		}
		if !chat.Pinned && existing.Pinned {
			chat.Pinned = existing.Pinned
		}
	}
	md.chats[chat.Id] = chat
}

// SetChatArchived updates the archived flag for a chat.
func (md *MessageDatabase) SetChatArchived(chatID string, archived bool) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Archived = archived
		md.chats[chatID] = chat
	}
}

// SetChatPinned updates the pinned flag for a chat.
func (md *MessageDatabase) SetChatPinned(chatID string, pinned bool) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Pinned = pinned
		md.chats[chatID] = chat
	}
}

// SetChatUnreadCount sets the unread counter for a chat without touching
// individual message flags. The count is only raised, never lowered, so
// that locally tracked counts aren't lost by a less precise app-state
// "mark unread" (which only tells us unread≥1). To clear, use MarkChatRead.
func (md *MessageDatabase) SetChatUnreadCount(chatID string, count int) {
	md.chatLock.Lock()
	defer md.chatLock.Unlock()
	if chat, ok := md.chats[chatID]; ok {
		if count > chat.Unread {
			chat.Unread = count
			md.chats[chatID] = chat
		}
	}
}

// RecomputeUnreadCounts recalculates chat unread counters from individual
// message Unread flags.  Call after loading the message cache to restore
// counts that survived across restarts.
//
// For chats with stored messages the flags are authoritative and the counter
// is SET, not raised: the chat cache and the message cache both persist
// unread state, so loading both would otherwise double the counter on every
// restart. Chats with no stored messages keep their cached counter (e.g. a
// count seeded from history sync for a chat whose messages we never stored).
func (md *MessageDatabase) RecomputeUnreadCounts() {
	md.messageLock.RLock()
	counts := make(map[string]int)
	for chatID, msgs := range md.messages {
		if len(msgs) == 0 {
			continue
		}
		n := 0
		for _, msg := range msgs {
			if msg.Unread {
				n++
			}
		}
		counts[chatID] = n
	}
	md.messageLock.RUnlock()

	md.chatLock.Lock()
	for chatID, count := range counts {
		if chat, ok := md.chats[chatID]; ok {
			chat.Unread = count
			md.chats[chatID] = chat
		}
	}
	md.chatLock.Unlock()
}

// UpdateChatUnread syncs unread counts from external sources such as history sync.
func (md *MessageDatabase) UpdateChatUnread(chatID string, unread int) {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	ids := md.lastIncomingMessageIDsLocked(chatID, unread)
	unreadSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		unreadSet[id] = struct{}{}
	}

	msgs := md.messages[chatID]
	for idx, msg := range msgs {
		_, ok := unreadSet[msg.Id]
		msg.Unread = ok
		msgs[idx] = msg
		if stored, found := md.messagesById[msg.Id]; found {
			stored.Unread = ok
			md.messagesById[msg.Id] = stored
		}
	}
	md.messages[chatID] = msgs

	md.chatLock.Lock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Unread = len(ids)
		md.chats[chatID] = chat
	}
	md.chatLock.Unlock()
}

func (md *MessageDatabase) lastIncomingMessageIDsLocked(chatID string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	msgs := md.messages[chatID]
	ids := make([]string, 0, limit)
	for idx := len(msgs) - 1; idx >= 0 && len(ids) < limit; idx-- {
		if !msgs[idx].FromMe {
			ids = append(ids, msgs[idx].Id)
		}
	}
	return ids
}

// PruneStaleUnread drops unread flags from messages older than cutoff (Unix
// seconds), and zeroes counters of chats with no activity since then. WhatsApp
// only replays missed read-self receipts for a bounded window after we
// reconnect, so a flag older than that window can never be reconciled with
// the phone again — and the phone has almost certainly read it. Without this,
// every long backend downtime leaves permanent phantom unread counts.
// Call before RecomputeUnreadCounts.
func (md *MessageDatabase) PruneStaleUnread(cutoff int64) {
	md.messageLock.Lock()
	for chatID, msgs := range md.messages {
		for idx, msg := range msgs {
			if msg.Unread && int64(msg.Timestamp) < cutoff {
				msg.Unread = false
				msgs[idx] = msg
				if stored, ok := md.messagesById[msg.Id]; ok {
					stored.Unread = false
					md.messagesById[msg.Id] = stored
				}
			}
		}
		md.messages[chatID] = msgs
	}
	md.messageLock.Unlock()

	md.chatLock.Lock()
	for chatID, chat := range md.chats {
		if chat.Unread > 0 && chat.LastMessage > 0 && chat.LastMessage < cutoff {
			chat.Unread = 0
			md.chats[chatID] = chat
		}
	}
	md.chatLock.Unlock()
}

// MarkChatReadUpTo clears unread flags for messages at or before cutoff
// (Unix seconds) and resets the chat counter to however many unread messages
// remain. A cutoff of 0 clears everything. Used when the phone tells us a
// chat was read at a specific time: messages that arrived after that moment
// stay unread, and — unlike SetChatUnreadCount — the counter is allowed to
// go DOWN, which is what makes phone-side reads actually converge here.
func (md *MessageDatabase) MarkChatReadUpTo(chatID string, cutoff int64) {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	msgs := md.messages[chatID]
	remaining := 0
	for idx, msg := range msgs {
		if !msg.Unread {
			continue
		}
		if cutoff == 0 || int64(msg.Timestamp) <= cutoff {
			msg.Unread = false
			msgs[idx] = msg
			if stored, ok := md.messagesById[msg.Id]; ok {
				stored.Unread = false
				md.messagesById[msg.Id] = stored
			}
		} else {
			remaining++
		}
	}
	md.messages[chatID] = msgs

	md.chatLock.Lock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Unread = remaining
		md.chats[chatID] = chat
	}
	md.chatLock.Unlock()
}

// MarkChatRead clears unread state for the given chat and returns the unread messages that were cleared.
func (md *MessageDatabase) MarkChatRead(chatID string) []Message {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	msgs := md.messages[chatID]
	cleared := make([]Message, 0)
	for idx, msg := range msgs {
		if msg.Unread {
			cleared = append(cleared, msg)
			msg.Unread = false
			msgs[idx] = msg
			stored := md.messagesById[msg.Id]
			stored.Unread = false
			md.messagesById[msg.Id] = stored
		}
	}
	md.messages[chatID] = msgs

	md.chatLock.Lock()
	if chat, ok := md.chats[chatID]; ok {
		chat.Unread = 0
		md.chats[chatID] = chat
	}
	md.chatLock.Unlock()

	return cleared
}

// UpgradeMessageStatus raises the delivery status of our own messages,
// never lowering it (receipts can arrive out of order). Returns the ids
// that actually changed so callers can skip no-op UI updates.
func (md *MessageDatabase) UpgradeMessageStatus(messageIDs []string, status MessageStatus) []string {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	changed := make([]string, 0, len(messageIDs))
	for _, id := range messageIDs {
		msg, ok := md.messagesById[id]
		if !ok || !msg.FromMe || statusRank(status) <= statusRank(msg.Status) {
			continue
		}
		msg.Status = status
		md.messagesById[id] = msg
		md.replaceMessageLocked(msg)
		changed = append(changed, id)
	}
	return changed
}

// MarkMessageRevoked updates a message to show that it was revoked.
func (md *MessageDatabase) MarkMessageRevoked(messageID string) bool {
	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	msg, ok := md.messagesById[messageID]
	if !ok {
		return false
	}
	msg.Text = "[message revoked]"
	msg.RawMessage = nil
	msg.Kind = MessageKindUnknown
	md.messagesById[messageID] = msg
	md.replaceMessageLocked(msg)
	return true
}

// AddContact adds or updates a contact in the database.
func (md *MessageDatabase) AddContact(contact Contact) {
	md.contactLock.Lock()
	defer md.contactLock.Unlock()

	existing, ok := md.contacts[contact.Id]
	if ok {
		if contact.Name == "" {
			contact.Name = existing.Name
		}
		if contact.Short == "" {
			contact.Short = existing.Short
		}
	}
	md.contacts[contact.Id] = contact
}

// GetChatIds returns chats sorted by most recent message first.
// Individual contacts with no known activity are excluded to avoid
// cluttering the list. Groups are always shown since you explicitly joined them.
func (md *MessageDatabase) GetChatIds() []Chat {
	md.messageLock.RLock()
	md.chatLock.RLock()

	allChats := make([]Chat, 0, len(md.chats))
	for _, chat := range md.chats {
		// @newsletter chats are WhatsApp Channels — the native app shows them
		// under Updates, never in the chat list, and they have no read-receipt
		// flow, so their unread counts only ever grow.
		if strings.HasSuffix(chat.Id, "@broadcast") || strings.HasSuffix(chat.Id, "@lid") || strings.HasSuffix(chat.Id, "@newsletter") {
			continue
		}
		if chat.IsGroup || chat.LastMessage > 0 || len(md.messages[chat.Id]) > 0 || chat.Unread > 0 {
			c := chat
			if msgs := md.messages[chat.Id]; len(msgs) > 0 {
				newest := int64(msgs[len(msgs)-1].Timestamp)
				if newest > c.LastMessage {
					c.LastMessage = newest
				}
			} else if !chat.IsGroup {
				c.LastMessage = 0
			}
			allChats = append(allChats, c)
		}
	}

	md.chatLock.RUnlock()
	md.messageLock.RUnlock()
	sort.Slice(allChats, func(i, j int) bool {
		if allChats[i].Pinned != allChats[j].Pinned {
			return allChats[i].Pinned
		}
		if allChats[i].LastMessage == allChats[j].LastMessage {
			return allChats[i].Name < allChats[j].Name
		}
		return allChats[i].LastMessage > allChats[j].LastMessage
	})
	return allChats
}

// GetMessages returns all messages for the given chat.
func (md *MessageDatabase) GetMessages(chatID string) []Message {
	md.messageLock.RLock()
	defer md.messageLock.RUnlock()

	msgs := md.messages[chatID]
	out := make([]Message, len(msgs))
	copy(out, msgs)
	return out
}

// GetMessage returns a single message by ID.
func (md *MessageDatabase) GetMessage(id string) (Message, bool) {
	md.messageLock.RLock()
	defer md.messageLock.RUnlock()
	msg, ok := md.messagesById[id]
	return msg, ok
}

// GetOldestMessage returns the oldest stored message in a chat.
func (md *MessageDatabase) GetOldestMessage(chatID string) (Message, bool) {
	md.messageLock.RLock()
	defer md.messageLock.RUnlock()
	msgs := md.messages[chatID]
	if len(msgs) == 0 {
		return Message{}, false
	}
	return msgs[0], true
}

// GetMessageInfo returns a human-readable description of a message.
func (md *MessageDatabase) GetMessageInfo(id string) string {
	msg, ok := md.GetMessage(id)
	if !ok {
		return "Message not found"
	}

	name := md.GetIdName(msg.ContactId)
	short := md.GetIdShort(msg.ContactId)
	direction := "←"
	if msg.FromMe {
		direction = "→"
	}

	kind := string(msg.Kind)
	if kind == "" {
		kind = string(MessageKindUnknown)
	}

	info := fmt.Sprintf(
		"ID: %s\nType: %s\nFrom: %s (%s) %s\nTime: %s\nChat: %s",
		msg.Id,
		kind,
		name,
		short,
		direction,
		time.Unix(int64(msg.Timestamp), 0).Format(time.RFC1123),
		msg.ChatId,
	)
	if msg.FileName != "" {
		info += "\nFile: " + msg.FileName
	}
	if msg.MimeType != "" {
		info += "\nMIME: " + msg.MimeType
	}
	if msg.SenderId != "" {
		info += "\nSender: " + msg.SenderId
	}
	return info
}

// GetIdName resolves a contact or chat ID to a display name.
func (md *MessageDatabase) GetIdName(id string) string {
	if id == "" {
		return "Unknown"
	}

	md.contactLock.RLock()
	contact, ok := md.contacts[id]
	md.contactLock.RUnlock()
	if ok {
		if contact.Name != "" {
			return contact.Name
		}
		if contact.Short != "" {
			return contact.Short
		}
	}

	md.chatLock.RLock()
	chat, ok := md.chats[id]
	md.chatLock.RUnlock()
	if ok && chat.Name != "" {
		return chat.Name
	}

	return strings.TrimSuffix(strings.TrimSuffix(id, CONTACTSUFFIX), GROUPSUFFIX)
}

// GetIdShort resolves a contact or chat ID to a short display name.
func (md *MessageDatabase) GetIdShort(id string) string {
	if id == "" {
		return "Unknown"
	}

	md.contactLock.RLock()
	contact, ok := md.contacts[id]
	md.contactLock.RUnlock()
	if ok {
		if contact.Short != "" {
			return contact.Short
		}
		if contact.Name != "" {
			return contact.Name
		}
	}

	md.chatLock.RLock()
	chat, ok := md.chats[id]
	md.chatLock.RUnlock()
	if ok && chat.Name != "" {
		return chat.Name
	}

	return strings.TrimSuffix(strings.TrimSuffix(id, CONTACTSUFFIX), GROUPSUFFIX)
}

type chatCacheEntry struct {
	Id          string `json:"id"`
	IsGroup     bool   `json:"is_group"`
	Name        string `json:"name"`
	LastMessage int64  `json:"last_message"`
	Archived    bool   `json:"archived,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Unread      int    `json:"unread,omitempty"`
}

// SaveChatCache persists the current chat list (IDs, names, timestamps)
// to a local JSON file so sort order survives restarts.
func (md *MessageDatabase) SaveChatCache() {
	md.chatLock.RLock()
	entries := make([]chatCacheEntry, 0, len(md.chats))
	for _, chat := range md.chats {
		if chat.LastMessage > 0 || chat.IsGroup {
			entries = append(entries, chatCacheEntry{
				Id:          chat.Id,
				IsGroup:     chat.IsGroup,
				Name:        chat.Name,
				LastMessage: chat.LastMessage,
				Archived:    chat.Archived,
				Pinned:      chat.Pinned,
				Unread:      chat.Unread,
			})
		}
	}
	md.chatLock.RUnlock()

	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(config.GetChatCacheFilePath(), data, 0o644)
}

// LoadChatCache restores chat metadata from the local cache file.
// Existing chats with newer timestamps are not overwritten.
func (md *MessageDatabase) LoadChatCache() {
	data, err := os.ReadFile(config.GetChatCacheFilePath())
	if err != nil {
		return
	}
	var entries []chatCacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for _, entry := range entries {
		md.AddChat(Chat{
			Id:          entry.Id,
			IsGroup:     entry.IsGroup,
			Name:        entry.Name,
			LastMessage: entry.LastMessage,
			Archived:    entry.Archived,
			Pinned:      entry.Pinned,
			Unread:      entry.Unread,
		})
	}
}

type messageCacheEntry struct {
	Id           string `json:"id"`
	ChatId       string `json:"chat_id"`
	SenderId     string `json:"sender_id,omitempty"`
	ContactId    string `json:"contact_id,omitempty"`
	ContactName  string `json:"contact_name,omitempty"`
	ContactShort string `json:"contact_short,omitempty"`
	Timestamp    uint64 `json:"ts"`
	FromMe       bool   `json:"from_me,omitempty"`
	Forwarded    bool   `json:"forwarded,omitempty"`
	Text         string `json:"text,omitempty"`
	Kind         string `json:"kind,omitempty"`
	MimeType     string `json:"mime,omitempty"`
	FileName     string `json:"file,omitempty"`
	RawProto     string `json:"raw,omitempty"`
	Unread       bool   `json:"unread,omitempty"`
	Status       string `json:"status,omitempty"`
}

// SaveMessageCache persists all in-memory messages to disk.
func (md *MessageDatabase) SaveMessageCache() {
	md.messageLock.RLock()
	entries := make([]messageCacheEntry, 0, len(md.messagesById))
	for _, msg := range md.messagesById {
		entry := messageCacheEntry{
			Id:           msg.Id,
			ChatId:       msg.ChatId,
			SenderId:     msg.SenderId,
			ContactId:    msg.ContactId,
			ContactName:  msg.ContactName,
			ContactShort: msg.ContactShort,
			Timestamp:    msg.Timestamp,
			FromMe:       msg.FromMe,
			Forwarded:    msg.Forwarded,
			Text:         msg.Text,
			Kind:         string(msg.Kind),
			MimeType:     msg.MimeType,
			FileName:     msg.FileName,
			Unread:       msg.Unread,
			Status:       string(msg.Status),
		}
		if msg.RawMessage != nil {
			if raw, err := proto.Marshal(msg.RawMessage); err == nil {
				entry.RawProto = base64.StdEncoding.EncodeToString(raw)
			}
		}
		entries = append(entries, entry)
	}
	md.messageLock.RUnlock()

	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	_ = os.WriteFile(config.GetMessageCacheFilePath(), data, 0o644)
}

// LoadMessageCache restores messages from the local cache file.
func (md *MessageDatabase) LoadMessageCache() {
	data, err := os.ReadFile(config.GetMessageCacheFilePath())
	if err != nil {
		return
	}
	var entries []messageCacheEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for _, entry := range entries {
		msg := Message{
			Id:           entry.Id,
			ChatId:       entry.ChatId,
			SenderId:     entry.SenderId,
			ContactId:    entry.ContactId,
			ContactName:  entry.ContactName,
			ContactShort: entry.ContactShort,
			Timestamp:    entry.Timestamp,
			FromMe:       entry.FromMe,
			Forwarded:    entry.Forwarded,
			Text:         entry.Text,
			Kind:         MessageKind(entry.Kind),
			MimeType:     entry.MimeType,
			FileName:     entry.FileName,
			Unread:       entry.Unread,
			Status:       MessageStatus(entry.Status),
		}
		if entry.RawProto != "" {
			if raw, err := base64.StdEncoding.DecodeString(entry.RawProto); err == nil {
				var pb waProto.Message
				if proto.Unmarshal(raw, &pb) == nil {
					msg.RawMessage = &pb
				}
			}
		}
		md.AddMessage(msg, entry.Unread)
	}
}

// RefreshContactNames updates ContactName and ContactShort on all in-memory
// messages using the current contact/chat database. Call after contacts have
// been fully loaded so that messages cached before contact resolution get
// proper display names.
func (md *MessageDatabase) RefreshContactNames() {
	type update struct {
		chatID string
		idx    int
		msgID  string
		name   string
		short  string
	}

	md.contactLock.RLock()
	contactSnap := make(map[string]Contact, len(md.contacts))
	for k, v := range md.contacts {
		contactSnap[k] = v
	}
	md.contactLock.RUnlock()

	md.chatLock.RLock()
	chatSnap := make(map[string]Chat, len(md.chats))
	for k, v := range md.chats {
		chatSnap[k] = v
	}
	md.chatLock.RUnlock()

	resolve := func(id string) (string, string) {
		if c, ok := contactSnap[id]; ok {
			n := c.Name
			if n == "" {
				n = c.Short
			}
			s := c.Short
			if s == "" {
				s = c.Name
			}
			return n, s
		}
		if ch, ok := chatSnap[id]; ok && ch.Name != "" {
			return ch.Name, ch.Name
		}
		return "", ""
	}

	md.messageLock.Lock()
	defer md.messageLock.Unlock()

	for chatID, msgs := range md.messages {
		for i, msg := range msgs {
			if msg.ContactId == "" {
				continue
			}
			name, short := resolve(msg.ContactId)
			if name != "" && name != msg.ContactId {
				msgs[i].ContactName = name
			}
			if short != "" && short != msg.ContactId && !strings.HasPrefix(short, "+") {
				msgs[i].ContactShort = short
			} else if name != "" && name != msg.ContactId {
				msgs[i].ContactShort = name
			}
			if stored, ok := md.messagesById[msg.Id]; ok {
				stored.ContactName = msgs[i].ContactName
				stored.ContactShort = msgs[i].ContactShort
				md.messagesById[msg.Id] = stored
			}
		}
		md.messages[chatID] = msgs
	}
}

// StoreTranslation saves a translation for a message ID.
func (md *MessageDatabase) StoreTranslation(messageID, text string) {
	md.translationLock.Lock()
	defer md.translationLock.Unlock()
	md.translations[messageID] = text
}

// GetTranslation returns the cached translation for a message ID.
func (md *MessageDatabase) GetTranslation(messageID string) (string, bool) {
	md.translationLock.RLock()
	defer md.translationLock.RUnlock()
	t, ok := md.translations[messageID]
	return t, ok
}

// DeleteTranslation removes the cached translation for a message ID, if
// any. Used when the user explicitly re-runs translation on a message:
// we wipe the old result so the fresh translation becomes the canonical
// one and a future read doesn't hit the stale cache. Idempotent - calling
// on an unknown ID is a no-op.
func (md *MessageDatabase) DeleteTranslation(messageID string) {
	md.translationLock.Lock()
	defer md.translationLock.Unlock()
	delete(md.translations, messageID)
}

// SaveTranslationCache persists translations to disk.
func (md *MessageDatabase) SaveTranslationCache() {
	md.translationLock.RLock()
	snap := make(map[string]string, len(md.translations))
	for k, v := range md.translations {
		snap[k] = v
	}
	md.translationLock.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = os.WriteFile(config.GetTranslationCacheFilePath(), data, 0o644)
}

// LoadTranslationCache restores translations from disk.
func (md *MessageDatabase) LoadTranslationCache() {
	data, err := os.ReadFile(config.GetTranslationCacheFilePath())
	if err != nil {
		return
	}
	var entries map[string]string
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	md.translationLock.Lock()
	defer md.translationLock.Unlock()
	for k, v := range entries {
		md.translations[k] = v
	}
}

// StoreTranscription saves a transcription for a message ID.
func (md *MessageDatabase) StoreTranscription(messageID, text string) {
	md.transcriptionLock.Lock()
	defer md.transcriptionLock.Unlock()
	md.transcriptions[messageID] = text
}

// GetTranscription returns the cached transcription for a message ID.
func (md *MessageDatabase) GetTranscription(messageID string) (string, bool) {
	md.transcriptionLock.RLock()
	defer md.transcriptionLock.RUnlock()
	t, ok := md.transcriptions[messageID]
	return t, ok
}

// SaveTranscriptionCache persists transcriptions to disk.
func (md *MessageDatabase) SaveTranscriptionCache() {
	md.transcriptionLock.RLock()
	snap := make(map[string]string, len(md.transcriptions))
	for k, v := range md.transcriptions {
		snap[k] = v
	}
	md.transcriptionLock.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	_ = os.WriteFile(config.GetTranscriptionCacheFilePath(), data, 0o644)
}

// LoadTranscriptionCache restores transcriptions from disk.
func (md *MessageDatabase) LoadTranscriptionCache() {
	data, err := os.ReadFile(config.GetTranscriptionCacheFilePath())
	if err != nil {
		return
	}
	var entries map[string]string
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	md.transcriptionLock.Lock()
	defer md.transcriptionLock.Unlock()
	for k, v := range entries {
		md.transcriptions[k] = v
	}
}
