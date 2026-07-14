package messages

import (
	"testing"
)

func TestGetChatIdsSortOrder(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	db.AddChat(Chat{Id: "alice@s.whatsapp.net", Name: "Alice", LastMessage: 1000})
	db.AddChat(Chat{Id: "bob@s.whatsapp.net", Name: "Bob", LastMessage: 3000})
	db.AddChat(Chat{Id: "charlie@s.whatsapp.net", Name: "Charlie", LastMessage: 2000})
	db.AddChat(Chat{Id: "nobody@s.whatsapp.net", Name: "Nobody", LastMessage: 0})
	db.AddChat(Chat{Id: "empty@s.whatsapp.net", Name: "Empty Contact", LastMessage: 0})
	db.AddChat(Chat{Id: "group1@g.us", Name: "Work Group", LastMessage: 2500, IsGroup: true})
	db.AddChat(Chat{Id: "group2@g.us", Name: "Old Group", LastMessage: 0, IsGroup: true})
	db.AddChat(Chat{Id: "recent@s.whatsapp.net", Name: "Recent Chat", LastMessage: 5000, Unread: 3})

	chats := db.GetChatIds()

	t.Logf("Got %d chats:", len(chats))
	for i, c := range chats {
		t.Logf("  %d. %s (LastMessage=%d, Unread=%d)", i+1, c.Name, c.LastMessage, c.Unread)
	}

	if len(chats) != 6 {
		t.Fatalf("Expected 6 chats (excluding 2 bare contacts with LastMessage=0), got %d", len(chats))
	}

	expected := []string{"Recent Chat", "Bob", "Work Group", "Charlie", "Alice", "Old Group"}
	for i, name := range expected {
		if chats[i].Name != name {
			t.Errorf("Position %d: expected %q, got %q", i+1, name, chats[i].Name)
		}
	}
}

func TestGetChatIdsKeepsUnreadZeroTimestamp(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	db.AddChat(Chat{Id: "unread@s.whatsapp.net", Name: "Unread Contact", LastMessage: 0, Unread: 2})
	db.AddChat(Chat{Id: "empty@s.whatsapp.net", Name: "Empty Contact", LastMessage: 0})

	chats := db.GetChatIds()
	if len(chats) != 1 {
		t.Fatalf("Expected 1 chat (only the one with unreads), got %d", len(chats))
	}
	if chats[0].Name != "Unread Contact" {
		t.Errorf("Expected 'Unread Contact', got %q", chats[0].Name)
	}
}

func TestGetChatIdsKeepsChatWithMessages(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	db.AddChat(Chat{Id: "withmsg@s.whatsapp.net", Name: "Has Messages", LastMessage: 0})
	db.AddMessage(Message{
		Id:        "msg1",
		ChatId:    "withmsg@s.whatsapp.net",
		Timestamp: 1234,
		Text:      "hello",
		Kind:      MessageKindText,
	}, false)

	db.AddChat(Chat{Id: "empty@s.whatsapp.net", Name: "No Messages", LastMessage: 0})

	chats := db.GetChatIds()
	if len(chats) != 1 {
		t.Fatalf("Expected 1 chat (only the one with messages), got %d", len(chats))
	}
	if chats[0].Name != "Has Messages" {
		t.Errorf("Expected 'Has Messages', got %q", chats[0].Name)
	}
}

// Unread-count convergence: these guard against the ratchets that made
// counts drift ever-upward and diverge from the phone.

func unreadOf(t *testing.T, db *MessageDatabase, chatID string) int {
	t.Helper()
	for _, c := range db.GetChatIds() {
		if c.Id == chatID {
			return c.Unread
		}
	}
	t.Fatalf("chat %s not in list", chatID)
	return -1
}

func TestAddMessageDoesNotDoubleCountRedelivery(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	msg := Message{Id: "m1", ChatId: "a@s.whatsapp.net", ContactName: "A", Timestamp: 100, Text: "hi", Kind: MessageKindText}
	db.AddMessage(msg, true)
	db.AddMessage(msg, true) // offline-sync / cache-reload redelivery

	if got := unreadOf(t, db, "a@s.whatsapp.net"); got != 1 {
		t.Errorf("expected unread 1 after redelivery, got %d", got)
	}
}

func TestRecomputeUnreadCountsIsAuthoritativeWithMessages(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	// Chat cache seeded an inflated counter (the pre-fix doubling), but the
	// message flags say only one message is unread.
	db.AddChat(Chat{Id: "a@s.whatsapp.net", Name: "A", LastMessage: 100, Unread: 40})
	db.AddMessage(Message{Id: "m1", ChatId: "a@s.whatsapp.net", Timestamp: 100, Text: "hi", Kind: MessageKindText}, true)
	db.AddMessage(Message{Id: "m2", ChatId: "a@s.whatsapp.net", Timestamp: 101, Text: "yo", Kind: MessageKindText}, false)

	// A chat with a cached count but no stored messages keeps its counter.
	db.AddChat(Chat{Id: "b@s.whatsapp.net", Name: "B", LastMessage: 100, Unread: 2})

	db.RecomputeUnreadCounts()

	if got := unreadOf(t, db, "a@s.whatsapp.net"); got != 1 {
		t.Errorf("expected flags to override cached counter (1), got %d", got)
	}
	if got := unreadOf(t, db, "b@s.whatsapp.net"); got != 2 {
		t.Errorf("expected message-less chat to keep cached counter (2), got %d", got)
	}
}

func TestMarkChatReadUpToRespectsCutoff(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	chat := "a@s.whatsapp.net"
	db.AddMessage(Message{Id: "old", ChatId: chat, ContactName: "A", Timestamp: 100, Text: "old", Kind: MessageKindText}, true)
	db.AddMessage(Message{Id: "new", ChatId: chat, ContactName: "A", Timestamp: 200, Text: "new", Kind: MessageKindText}, true)

	// Phone read the chat at t=150: the old message clears, the new stays.
	db.MarkChatReadUpTo(chat, 150)

	if got := unreadOf(t, db, chat); got != 1 {
		t.Errorf("expected 1 unread after cutoff clear, got %d", got)
	}
	if msg, _ := db.GetMessage("old"); msg.Unread {
		t.Error("old message should have been cleared")
	}
	if msg, _ := db.GetMessage("new"); !msg.Unread {
		t.Error("new message should still be unread")
	}

	// Cutoff 0 = clear everything, and the counter may go DOWN.
	db.MarkChatReadUpTo(chat, 0)
	if got := unreadOf(t, db, chat); got != 0 {
		t.Errorf("expected 0 unread after full clear, got %d", got)
	}
}

func TestMarkChatReadUpToLowersPhantomCounter(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	// Poisoned cache: big counter, no message flags to back it.
	db.AddChat(Chat{Id: "a@s.whatsapp.net", Name: "A", LastMessage: 100, Unread: 99})

	db.MarkChatReadUpTo("a@s.whatsapp.net", 0)

	if got := unreadOf(t, db, "a@s.whatsapp.net"); got != 0 {
		t.Errorf("expected phantom counter cleared to 0, got %d", got)
	}
}

func TestGetChatIdsFiltersNewsletters(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	db.AddChat(Chat{Id: "12036@newsletter", Name: "Channel", LastMessage: 100, Unread: 500})
	db.AddChat(Chat{Id: "a@s.whatsapp.net", Name: "A", LastMessage: 100})

	chats := db.GetChatIds()
	if len(chats) != 1 || chats[0].Id != "a@s.whatsapp.net" {
		t.Errorf("expected newsletter filtered out, got %v", chats)
	}
}

func TestPruneStaleUnread(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()

	chat := "a@s.whatsapp.net"
	db.AddMessage(Message{Id: "ancient", ChatId: chat, ContactName: "A", Timestamp: 100, Text: "old", Kind: MessageKindText}, true)
	db.AddMessage(Message{Id: "fresh", ChatId: chat, ContactName: "A", Timestamp: 900, Text: "new", Kind: MessageKindText}, true)
	// Phantom counter on a chat whose last activity predates the horizon.
	db.AddChat(Chat{Id: "dead@s.whatsapp.net", Name: "Dead", LastMessage: 50, Unread: 77})
	// Phantom counter on a still-active chat is left for receipts to resolve.
	db.AddChat(Chat{Id: "live@s.whatsapp.net", Name: "Live", LastMessage: 950, Unread: 3})

	db.PruneStaleUnread(500)
	db.RecomputeUnreadCounts()

	if got := unreadOf(t, db, chat); got != 1 {
		t.Errorf("expected only the fresh message to stay unread, got %d", got)
	}
	if msg, _ := db.GetMessage("ancient"); msg.Unread {
		t.Error("ancient message should have been pruned")
	}
	if got := unreadOf(t, db, "dead@s.whatsapp.net"); got != 0 {
		t.Errorf("expected stale phantom counter zeroed, got %d", got)
	}
	if got := unreadOf(t, db, "live@s.whatsapp.net"); got != 3 {
		t.Errorf("expected active chat counter kept, got %d", got)
	}
}
