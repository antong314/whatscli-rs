package messages

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const testGroupID = "120363203127855640@g.us"

func newHistoryTestSession() (*SessionManager, *eventHandler) {
	db := &MessageDatabase{}
	db.Init()
	sm := &SessionManager{db: db, uiHandler: noopUI{}}
	// An offline client is enough for ParseWebMessage on group messages.
	sm.client = whatsmeow.NewClient(&store.Device{}, nil)
	eh := &eventHandler{sm: sm}
	sm.eventHandler = eh
	return sm, eh
}

func groupHistoryMsg(id, participant string, ts uint64, msg *waProto.Message) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{
			RemoteJID:   proto.String(testGroupID),
			ID:          proto.String(id),
			FromMe:      proto.Bool(false),
			Participant: proto.String(participant),
		},
		Participant:      proto.String(participant),
		MessageTimestamp: proto.Uint64(ts),
		Message:          msg,
	}}
}

func receivePage(t *testing.T, ch <-chan historyPage) historyPage {
	t.Helper()
	select {
	case page := <-ch:
		return page
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not woken")
		return historyPage{}
	}
}

func TestOnDemandHistorySyncStoresContactCardAndWakesWaiter(t *testing.T) {
	sm, eh := newHistoryTestSession()
	pages, unsubscribe := sm.subscribeHistory(testGroupID)
	defer unsubscribe()
	otherPages, unsubscribeOther := sm.subscribeHistory("120363184598656143@g.us")
	defer unsubscribeOther()

	response := &events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_ON_DEMAND.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID:   proto.String(testGroupID),
			Name: proto.String("San Mateo Love"),
			Messages: []*waHistorySync.HistorySyncMsg{
				groupHistoryMsg("contact-1", "15551234567@s.whatsapp.net", 2000, &waProto.Message{
					ContactMessage: &waProto.ContactMessage{
						DisplayName: proto.String("Jane Doe"),
						Vcard:       proto.String(testVCard),
					},
				}),
				groupHistoryMsg("text-1", "15557654321@s.whatsapp.net", 1000, &waProto.Message{
					Conversation: proto.String("anyone know a plumber?"),
				}),
			},
		}},
	}}
	eh.handleHistorySync(response)

	stored, ok := sm.db.GetMessage("contact-1")
	if !ok {
		t.Fatal("contact card from history sync was dropped")
	}
	if stored.Kind != MessageKindContact || stored.Text != "[CONTACT] Jane Doe" || stored.ChatId != testGroupID {
		t.Fatalf("unexpected stored contact: %#v", stored)
	}
	if stored.RawMessage.GetContactMessage().GetVcard() != testVCard {
		t.Fatal("history contact lost its vCard")
	}

	page := receivePage(t, pages)
	if page.Returned != 2 || page.Added != 2 {
		t.Fatalf("expected 2 returned / 2 added, got %#v", page)
	}
	if page.Oldest.ID != "text-1" || page.Oldest.Timestamp != 1000 {
		t.Fatalf("expected oldest anchor text-1@1000, got %#v", page.Oldest)
	}
	if miss := receivePage(t, otherPages); miss.Returned != 0 {
		t.Fatalf("a chat absent from the response should get an empty page, got %#v", miss)
	}

	// The same response again only deduplicates.
	eh.handleHistorySync(response)
	if page := receivePage(t, pages); page.Returned != 2 || page.Added != 0 {
		t.Fatalf("re-delivered history must dedupe by ID, got %#v", page)
	}
	if got := len(sm.db.GetMessages(testGroupID)); got != 2 {
		t.Fatalf("expected 2 cached messages, got %d", got)
	}
	<-otherPages
}

func TestNonOnDemandHistorySyncDoesNotWakeWaiters(t *testing.T) {
	sm, eh := newHistoryTestSession()
	pages, unsubscribe := sm.subscribeHistory(testGroupID)
	defer unsubscribe()
	eh.handleHistorySync(&events.HistorySync{Data: &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID:   proto.String(testGroupID),
			Name: proto.String("San Mateo Love"),
			Messages: []*waHistorySync.HistorySyncMsg{
				groupHistoryMsg("recent-1", "15551234567@s.whatsapp.net", 1000, &waProto.Message{Conversation: proto.String("hi")}),
			},
		}},
	}})
	select {
	case page := <-pages:
		t.Fatalf("pairing-time history must not be mistaken for an on-demand page: %#v", page)
	default:
	}
}

// fakePhone answers on-demand requests from a fixed per-chat history,
// inserting messages and publishing pages the way handleHistorySync does.
type fakePhone struct {
	sm       *SessionManager
	history  []Message // ascending by timestamp
	mu       sync.Mutex
	requests []historyAnchor
}

func newFakePhone(sm *SessionManager, chatID string, count int, base uint64) *fakePhone {
	phone := &fakePhone{sm: sm}
	for i := 0; i < count; i++ {
		phone.history = append(phone.history, Message{
			Id:        fmt.Sprintf("m%03d", i),
			ChatId:    chatID,
			Timestamp: base + uint64(i)*60,
			Kind:      MessageKindText,
			Text:      fmt.Sprintf("message %d", i),
		})
	}
	sm.historyRequestFn = phone.request
	return phone
}

func (p *fakePhone) request(chatID string, anchor historyAnchor, count int) error {
	p.mu.Lock()
	p.requests = append(p.requests, anchor)
	p.mu.Unlock()
	idx := -1
	for i, msg := range p.history {
		if msg.Id == anchor.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil // phone doesn't know the anchor: silently no response
	}
	start := idx - count
	if start < 0 {
		start = 0
	}
	batch := p.history[start:idx]
	go func() {
		if len(batch) == 0 {
			p.sm.publishHistoryMisses(nil)
			return
		}
		page := historyPage{ChatID: chatID, Returned: len(batch), Oldest: historyAnchor{ID: batch[0].Id, Timestamp: batch[0].Timestamp}}
		for _, msg := range batch {
			if p.sm.db.AddMessage(msg, false) {
				page.Added++
			}
		}
		p.sm.publishHistoryPage(page, nil, chatID)
	}()
	return nil
}

func refetchTestOptions(t *testing.T, chatID string) HistoryRefetchOptions {
	return HistoryRefetchOptions{
		ChatIDs:  []string{chatID},
		PageSize: 50,
		Timeout:  time.Second,
		Delay:    0,
		Logf:     t.Logf,
	}
}

func TestRefetchHistoryPagesBackUntilSince(t *testing.T) {
	sm, _ := newHistoryTestSession()
	phone := newFakePhone(sm, testGroupID, 300, 1_767_000_000)
	// The cache holds the newest 20 messages plus a stray older one.
	for _, msg := range phone.history[280:] {
		sm.db.AddMessage(msg, false)
	}
	sm.db.AddMessage(phone.history[150], false)

	opts := refetchTestOptions(t, testGroupID)
	opts.Since = time.Unix(int64(phone.history[120].Timestamp), 0)
	results := sm.RefetchHistory(context.Background(), opts)
	if len(results) != 1 {
		t.Fatalf("expected one result, got %#v", results)
	}
	r := results[0]
	if r.Err != nil || r.StopReason != "reached --since" {
		t.Fatalf("expected clean stop at --since, got %#v", r)
	}
	// Pages: 249-298, 199-248, 149-198, 99-148.
	if r.Pages != 4 || r.Returned != 200 {
		t.Fatalf("expected 4 pages / 200 returned, got pages=%d returned=%d", r.Pages, r.Returned)
	}
	// 99..279 were missing except the stray #150.
	if r.Added != 180 {
		t.Fatalf("expected 180 added, got %d", r.Added)
	}
	if r.OldestTimestamp != phone.history[99].Timestamp {
		t.Fatalf("oldest reached should be #99, got %d", r.OldestTimestamp)
	}
	if got := len(sm.db.GetMessages(testGroupID)); got != 201 {
		t.Fatalf("expected 201 cached messages (99..299), got %d", got)
	}
	if first := phone.requests[0]; first.ID != "m299" {
		t.Fatalf("first request must anchor on the newest cached message, got %#v", first)
	}
}

func TestRefetchHistoryStopsWhenPhoneHasNothingOlder(t *testing.T) {
	sm, _ := newHistoryTestSession()
	phone := newFakePhone(sm, testGroupID, 120, 1_767_000_000)
	sm.db.AddMessage(phone.history[119], false)

	results := sm.RefetchHistory(context.Background(), refetchTestOptions(t, testGroupID))
	r := results[0]
	if r.Err != nil || r.StopReason != "phone returned no older messages" {
		t.Fatalf("expected stop on empty response, got %#v", r)
	}
	// 69-118, 19-68, 0-18, then an empty response.
	if r.Pages != 4 || r.Added != 119 {
		t.Fatalf("expected 4 pages / 119 added, got pages=%d added=%d", r.Pages, r.Added)
	}
	if got := len(sm.db.GetMessages(testGroupID)); got != 120 {
		t.Fatalf("expected the whole history cached, got %d", got)
	}
}

func TestRefetchHistoryFallsBackWhenNewestAnchorIsUnknown(t *testing.T) {
	sm, _ := newHistoryTestSession()
	phone := newFakePhone(sm, testGroupID, 60, 1_767_000_000)
	sm.db.AddMessage(phone.history[59], false)
	// Newest cached message the phone doesn't know (e.g. deleted there).
	sm.db.AddMessage(Message{Id: "ghost", ChatId: testGroupID, Timestamp: phone.history[59].Timestamp + 60, Kind: MessageKindText}, false)

	opts := refetchTestOptions(t, testGroupID)
	opts.Timeout = 100 * time.Millisecond
	r := sm.RefetchHistory(context.Background(), opts)[0]
	if r.Err != nil {
		t.Fatalf("fallback anchor should succeed, got %#v", r)
	}
	if phone.requests[0].ID != "ghost" || phone.requests[1].ID != "m059" {
		t.Fatalf("expected ghost then m059 anchors, got %#v", phone.requests[:2])
	}
	if r.Added != 59 {
		t.Fatalf("expected 59 added via the fallback anchor, got %d", r.Added)
	}
}

func TestRefetchHistoryReportsChatWithoutAnchor(t *testing.T) {
	sm, _ := newHistoryTestSession()
	phone := newFakePhone(sm, testGroupID, 10, 1_767_000_000)
	opts := refetchTestOptions(t, "120363999999999999@g.us")
	opts.Timeout = 50 * time.Millisecond
	r := sm.RefetchHistory(context.Background(), opts)[0]
	if r.Err == nil || r.StopReason != "no cached message to anchor on" {
		t.Fatalf("expected a no-anchor error, got %#v", r)
	}
	// One best-effort blank anchor was tried (no session DB in tests).
	if len(phone.requests) != 1 || phone.requests[0].ID != "" || phone.requests[0].Timestamp == 0 {
		t.Fatalf("expected a single blank fallback anchor, got %#v", phone.requests)
	}
}

func TestRefetchHistoryRespectsMaxPages(t *testing.T) {
	sm, _ := newHistoryTestSession()
	phone := newFakePhone(sm, testGroupID, 500, 1_767_000_000)
	sm.db.AddMessage(phone.history[499], false)
	opts := refetchTestOptions(t, testGroupID)
	opts.MaxPages = 2
	r := sm.RefetchHistory(context.Background(), opts)[0]
	if r.Pages != 2 || r.Added != 100 || r.StopReason != "reached max pages (2)" {
		t.Fatalf("expected a 2-page cap, got %#v", r)
	}
}
