package messages

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

func TestContactForMessageUsesPushNameForUnsavedGroupSender(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()
	eh := &eventHandler{sm: &SessionManager{db: db}}
	sender := types.NewADJID("15551234567", 0, 25)

	id, name, short := eh.contactForMessage(types.MessageInfo{
		MessageSource: types.MessageSource{Sender: sender, IsGroup: true},
		PushName:      " Alex Example ",
	})

	if id != "15551234567@s.whatsapp.net" {
		t.Fatalf("expected canonical sender id, got %q", id)
	}
	if name != "Alex Example" || short != "Alex Example" {
		t.Fatalf("expected push name for unsaved participant, got name=%q short=%q", name, short)
	}
}

func TestContactForMessageKeepsSavedNameAheadOfPushName(t *testing.T) {
	db := &MessageDatabase{}
	db.Init()
	id := "15551234567@s.whatsapp.net"
	db.AddContact(Contact{Id: id, Name: "Saved Alex", Short: "Alex"})
	eh := &eventHandler{sm: &SessionManager{db: db}}

	_, name, short := eh.contactForMessage(types.MessageInfo{
		MessageSource: types.MessageSource{
			Sender:  types.NewJID("15551234567", types.DefaultUserServer),
			IsGroup: true,
		},
		PushName: "Alex Example",
	})

	if name != "Saved Alex" || short != "Alex" {
		t.Fatalf("saved contact must win, got name=%q short=%q", name, short)
	}
}

func TestMessageScreensEqualIgnoresRawMessagePointer(t *testing.T) {
	a := []Message{{Id: "m1", ChatId: "chat", Text: "hello", Kind: MessageKindText, RawMessage: &waProto.Message{}}}
	b := []Message{{Id: "m1", ChatId: "chat", Text: "hello", Kind: MessageKindText, RawMessage: &waProto.Message{}}}

	if !messageScreensEqual(a, b) {
		t.Fatal("backend-only raw message pointers must not cause a GUI refresh")
	}
	b[0].ContactName = "New push name"
	if messageScreensEqual(a, b) {
		t.Fatal("a visible sender-name upgrade must refresh the open thread")
	}
}

func TestChatScreensEqualDetectsVisibleChanges(t *testing.T) {
	a := []Chat{{Id: "chat", Name: "Alice", Unread: 1, LastMessage: 100}}
	b := append([]Chat(nil), a...)
	if !chatScreensEqual(a, b) {
		t.Fatal("identical chat snapshots should be suppressed")
	}
	b[0].Unread = 0
	if chatScreensEqual(a, b) {
		t.Fatal("an unread-count change must refresh the chat list")
	}
}

// mkIncoming builds an incoming text message fixture for the
// decideAutoTranslateOutgoing tests. Only the fields the gate actually
// reads (FromMe, Text) need to be populated.
func mkIncoming(text string) Message { return Message{Text: text, FromMe: false} }

// mkOutgoing builds an outgoing text message fixture. The gate ignores
// outgoing messages entirely - this exists so the fixtures look like
// realistic interleaved threads instead of unrealistic incoming-only
// streams.
func mkOutgoing(text string) Message { return Message{Text: text, FromMe: true} }

// Real-world regression: the "Allan Pool piscina Oretina" thread has
// only 3 incoming messages over 2 months - 2 Spanish reminders followed
// by 1 English message - and the user's English reply was incorrectly
// sent verbatim instead of translated. Pin the fix: a contact who is
// 2-of-3 (66%) Spanish historically gets the auto-translate even when
// their most recent message was English, because slow-burn threads
// don't have enough sample size for the latest-message heuristic to
// be trustworthy.
func TestDecideAutoTranslateOutgoing_AllanThread_Translates(t *testing.T) {
	thread := []Message{
		mkIncoming("Hola Antón, para recordarte el mantenimiento de la piscina gracias"),
		mkOutgoing("Muchas gracias"),
		mkIncoming("Hola Anton, y disculpas por la respuesta tardía. Parece que los días tres meses pasamos. ¿Cuál tener algo?"),
		mkIncoming("Hi Jefes, just a reminder about the pool experience. Best regards"),
	}
	if !decideAutoTranslateOutgoing(thread) {
		t.Errorf("Allan thread (2 Spanish + 1 English from a Spanish-default contact) should auto-translate; got false")
	}
}

// Real-world regression: the "Padres de Lydia y Claire" thread is
// English with a single mostly-English message that opened with "Hola
// hola,". Auto-translating an English send into this thread would be
// the user's reported bug - confirm we don't.
func TestDecideAutoTranslateOutgoing_PadresThread_DoesNotTranslate(t *testing.T) {
	thread := []Message{
		mkIncoming("Hi! We will be by Casa Vic in a few minutes"),
		mkIncoming("Happy Monday! In preparation for a pick up time, would it be okay if Lydia come to us?"),
		mkIncoming("Hahaha ! I love that 😂 yess it's ok for us"),
		mkIncoming("I love are proactive you are 👌"),
		mkIncoming("I've to admit that the stress level of the pick up time can rise quickly when two or three kids are shouting at the same time when we are trying to spot other parents to get their approval 😅"),
		mkIncoming("I'm happy to alíviate your stress level today"),
		mkIncoming("I got Lydia, all good. I think I passed you without acknowledging because I was late …"),
		mkIncoming("You where pretty focus indeed 😂"),
		mkIncoming("Hola hola, Is 7h30 good for you or you would prefer now?"),
		mkIncoming("Yep"),
		mkIncoming("Good morning! Girls are asking if they can come to you today after school?"),
		mkIncoming("Yeah for sure we will take her home after school 😜"),
		mkIncoming("Thank you!"),
	}
	if decideAutoTranslateOutgoing(thread) {
		t.Errorf("English thread with one Spanish-greeting message must NOT auto-translate; got true")
	}
}

// A genuinely mixed-language contact (some English, some Spanish, both
// substantial) where the *most recent* incoming message was English:
// we're in the middle of an English exchange, defer to that and don't
// auto-translate the user's English reply into Spanish. This is the
// case the latest-message tiebreaker was designed for.
//
// "Mixed" here means below the 60% all-time threshold but above the
// 40% confident-thread bar - i.e. exactly the regime where we should
// take the latest-message hint.
func TestDecideAutoTranslateOutgoing_MixedContact_LatestEnglish_DoesNotTranslate(t *testing.T) {
	thread := []Message{
		mkIncoming("Hola, ¿cómo estás?"),
		mkIncoming("Mañana voy al supermercado por la tarde"),
		mkIncoming("Necesito comprar pan, leche y huevos"),
		mkIncoming("¿Vamos a la reunión a las tres?"),
		mkIncoming("hi how are you doing today"),
		mkIncoming("are you free this weekend for a coffee"),
		mkIncoming("I think we should meet up sometime soon"),
		mkIncoming("let me know what works best for your schedule"),
		mkIncoming("ok sounds good talk to you later"),
	}
	if decideAutoTranslateOutgoing(thread) {
		t.Errorf("mixed contact whose latest message was English must NOT auto-translate; got true")
	}
}

// Same mixed-language contact, but the most recent message was Spanish
// - flips the latest-message tiebreaker the other way.
func TestDecideAutoTranslateOutgoing_MixedContact_LatestSpanish_Translates(t *testing.T) {
	thread := []Message{
		mkIncoming("hi how are you doing today"),
		mkIncoming("are you free this weekend for a coffee"),
		mkIncoming("I think we should meet up sometime soon"),
		mkIncoming("let me know what works best for your schedule"),
		mkIncoming("ok sounds good talk to you later"),
		mkIncoming("Hola, ¿cómo estás?"),
		mkIncoming("Mañana voy al supermercado por la tarde"),
		mkIncoming("Necesito comprar pan, leche y huevos"),
		mkIncoming("¿Vamos a la reunión a las tres?"),
	}
	if !decideAutoTranslateOutgoing(thread) {
		t.Errorf("mixed contact whose latest message was Spanish should auto-translate; got false")
	}
}

// All-English thread - obviously must not auto-translate.
func TestDecideAutoTranslateOutgoing_AllEnglish_DoesNotTranslate(t *testing.T) {
	thread := []Message{
		mkIncoming("hey what's up"),
		mkIncoming("how was your weekend"),
		mkIncoming("we should grab dinner soon"),
		mkIncoming("let me know when you're free"),
		mkIncoming("looking forward to catching up"),
	}
	if decideAutoTranslateOutgoing(thread) {
		t.Errorf("all-English thread must NOT auto-translate; got true")
	}
}

// Empty / brand-new chat: no signal, never translate.
func TestDecideAutoTranslateOutgoing_EmptyThread_DoesNotTranslate(t *testing.T) {
	if decideAutoTranslateOutgoing(nil) {
		t.Errorf("empty thread must NOT auto-translate; got true")
	}
	if decideAutoTranslateOutgoing([]Message{}) {
		t.Errorf("empty-slice thread must NOT auto-translate; got true")
	}
}

// Outgoing-only thread (user sent a few messages, contact never
// replied): no signal from the contact, must not auto-translate.
func TestDecideAutoTranslateOutgoing_OutgoingOnly_DoesNotTranslate(t *testing.T) {
	thread := []Message{
		mkOutgoing("Hola, ¿estás disponible?"),
		mkOutgoing("Gracias!"),
		mkOutgoing("Mañana voy a pasar por la oficina"),
	}
	if decideAutoTranslateOutgoing(thread) {
		t.Errorf("thread with no incoming messages must NOT auto-translate; got true")
	}
}
