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
