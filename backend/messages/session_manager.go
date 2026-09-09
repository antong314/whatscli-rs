package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/antong314/whatscli-rs/backend/config"
	"github.com/antong314/whatscli-rs/backend/transcribe"
	"github.com/antong314/whatscli-rs/backend/translate"
	"github.com/gen2brain/beeep"
	_ "github.com/mattn/go-sqlite3" // SQLite driver
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var urlPattern = regexp.MustCompile(`https?://[^\s]+`)

// SessionManager deals with the connection and receives commands from the UI.
type SessionManager struct {
	db              *MessageDatabase
	currentReceiver string
	uiHandler       UiMessageHandler
	client          *whatsmeow.Client
	container       *sqlstore.Container
	BatteryChannel  chan BatteryMsg
	StatusChannel   chan StatusMsg
	CommandChannel  chan Command
	ChatChannel     chan Chat
	ContactChannel  chan Contact
	TextChannel     chan *waProto.Message
	statusInfo      SessionStatus
	lastSent        time.Time
	started         bool
	eventHandler    *eventHandler
	Translator      *translate.Translator
	Transcriber     *transcribe.Transcriber

	// markReadDelay is how long the user must dwell on a chat before a PROBE
	// selection is converted to an actual read receipt. Tunable for tests.
	// COMMIT selections (Enter on a chat row) bypass this entirely.
	markReadDelay time.Duration
	// markReadMu guards markReadTimer / markReadTimerChat. Held only across
	// short critical sections (timer create/stop and a single map lookup).
	markReadMu sync.Mutex
	// markReadTimer is the pending auto-mark-as-read timer scheduled by the
	// most recent PROBE selection. Cancelled and replaced on every new
	// setCurrentReceiver. nil when no timer is pending.
	markReadTimer *time.Timer
	// markReadTimerChat is the chat ID the pending timer would mark on fire;
	// captured at schedule time so a late-arriving timer can no-op if the
	// user has since moved on to a different chat.
	markReadTimerChat string
	// markReadFn is the function actually invoked to perform a mark-as-read.
	// Defaults to sm.autoMarkRead in Init. Tests can swap this out to count
	// invocations without standing up a real WhatsApp client.
	markReadFn func(chatID string)

	// avatarMu guards avatarCache. WhatsApp rate-limits profile-picture
	// lookups, so results — including "has no picture" — are cached for the
	// lifetime of the session.
	avatarMu    sync.Mutex
	avatarCache map[string]avatarEntry

	// chatRefreshMu guards chatRefreshTimer, which coalesces bursts of
	// read-state events (a full app-state resync emits one per chat) into a
	// single chat-list push and cache save instead of hundreds.
	chatRefreshMu          sync.Mutex
	chatRefreshTimer       *time.Timer
	chatRefreshPushPending bool

	// historyRefreshAt throttles lightweight per-selection history refreshes.
	// Cached messages render immediately; the phone response supplies mutable
	// metadata such as reactions that the older disk cache may not contain.
	historyRefreshMu sync.Mutex
	historyRefreshAt map[string]time.Time

	// userDisconnected is set when the user explicitly disconnects, so the
	// connection watchdog doesn't fight their intent by reconnecting.
	userDisconnected bool
}

// avatarEntry is one cached profile-picture lookup. err is non-nil for
// negative results (no picture set / not visible to us).
type avatarEntry struct {
	data []byte
	mime string
	err  error
}

// ErrNoAvatar marks chats without a (visible) profile picture so the gRPC
// layer can map them to NOT_FOUND instead of a hard error.
var ErrNoAvatar = errors.New("chat has no profile picture")

// DefaultMarkReadDelay is how long we wait after a user PROBE-selects a chat
// (e.g. via Up/Down arrow) before marking it as read. Picked to be longer
// than typical "scrolling past" but shorter than "I'm reading the messages".
const DefaultMarkReadDelay = 3 * time.Second

// StoreTranslation saves a translation for a message ID (thread-safe, persistent).
func (sm *SessionManager) StoreTranslation(messageID, text string) {
	sm.db.StoreTranslation(messageID, text)
}

// GetTranslation returns the cached translation for a message ID.
func (sm *SessionManager) GetTranslation(messageID string) (string, bool) {
	return sm.db.GetTranslation(messageID)
}

// StoreTranscription saves a transcription for a message ID (thread-safe, persistent).
func (sm *SessionManager) StoreTranscription(messageID, text string) {
	sm.db.StoreTranscription(messageID, text)
}

// GetTranscription returns the cached transcription for a message ID.
func (sm *SessionManager) GetTranscription(messageID string) (string, bool) {
	return sm.db.GetTranscription(messageID)
}

// DownloadImage downloads an image message to the preview directory and returns
// the file path. It reuses the existing download+cache logic so that already
// downloaded files are returned immediately.
func (sm *SessionManager) DownloadImage(msg Message) (string, error) {
	return sm.downloadMessage(msg, true)
}

// GetMessage returns a message by ID from the database.
func (sm *SessionManager) GetMessageByID(id string) (Message, bool) {
	return sm.db.GetMessage(id)
}

// Init initializes the SessionManager.
func (sm *SessionManager) Init(handler UiMessageHandler) {
	sm.db = &MessageDatabase{}
	sm.db.Init()
	sm.db.LoadTranslationCache()
	sm.db.LoadTranscriptionCache()
	sm.uiHandler = handler
	sm.BatteryChannel = make(chan BatteryMsg, 10)
	sm.StatusChannel = make(chan StatusMsg, 10)
	sm.CommandChannel = make(chan Command, 10)
	sm.ChatChannel = make(chan Chat, 10)
	sm.ContactChannel = make(chan Contact, 10)
	sm.TextChannel = make(chan *waProto.Message, 10)
	sm.eventHandler = &eventHandler{sm: sm}
	sm.markReadDelay = DefaultMarkReadDelay
	sm.markReadFn = sm.autoMarkRead
	sm.historyRefreshAt = make(map[string]time.Time)
}

// StartManager starts the receiver and message handling goroutine.
func (sm *SessionManager) StartManager() error {
	if sm.started {
		return errors.New("session manager running, send commands to control")
	}
	sm.started = true
	go sm.runManager()
	go sm.connectionWatchdog()
	return nil
}

// connectionWatchdog reconnects a paired session whose socket has silently
// died. Observed in the wild: after days of sleep/wake cycles the WhatsApp
// socket can drop without whatsmeow's auto-reconnect ever firing — the
// process sits with zero TCP connections while the UI still says
// "Connected", and no new messages arrive until a manual restart. The
// watchdog turns that permanent stall into at most a one-minute gap; the
// server's offline queue then delivers whatever was missed.
func (sm *SessionManager) connectionWatchdog() {
	for range time.Tick(time.Minute) {
		c := sm.client
		if c == nil || sm.userDisconnected || c.IsConnected() {
			continue
		}
		if c.Store == nil || c.Store.ID == nil {
			continue // not paired — the QR flow owns connecting
		}
		// Tell the UI the truth before trying to repair it.
		sm.StatusChannel <- StatusMsg{false, nil}
		sm.uiHandler.PrintText("Connection to WhatsApp lost — reconnecting…")
		if err := c.Connect(); err != nil && !errors.Is(err, whatsmeow.ErrAlreadyConnected) {
			sm.uiHandler.PrintError(fmt.Errorf("reconnect failed (will retry): %v", err))
		}
	}
}

func (sm *SessionManager) runManager() error {
	client, err := sm.getConnection()
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to create WhatsApp connection: %v", err))
		return err
	}
	if client == nil {
		return errors.New("could not establish WhatsApp connection")
	}

	if err = sm.loginWithConnection(client); err != nil {
		sm.uiHandler.PrintError(err)
	}

	for sm.started {
		select {
		case command := <-sm.CommandChannel:
			sm.execCommand(command)
		case batteryMsg := <-sm.BatteryChannel:
			sm.statusInfo.BatteryLoading = batteryMsg.loading
			sm.statusInfo.BatteryPowersave = batteryMsg.powersave
			sm.statusInfo.BatteryCharge = batteryMsg.charge
			sm.uiHandler.SetStatus(sm.statusInfo)
		case statusMsg := <-sm.StatusChannel:
			prevStatus := sm.statusInfo.Connected
			if statusMsg.err == nil {
				sm.statusInfo.Connected = statusMsg.connected
			}
			if sm.client != nil {
				sm.statusInfo.Connected = sm.client.IsConnected()
			} else {
				sm.statusInfo.Connected = false
			}
			sm.uiHandler.SetStatus(sm.statusInfo)
			if prevStatus != sm.statusInfo.Connected {
				if sm.statusInfo.Connected {
					sm.uiHandler.PrintText("connected")
				} else {
					sm.uiHandler.PrintText("disconnected")
				}
			}
		}
	}

	sm.uiHandler.PrintText("closing the receiver")
	if sm.client != nil {
		sm.client.Disconnect()
	}
	return nil
}

func (sm *SessionManager) setCurrentReceiver(id string, intent SelectIntent) {
	sm.currentReceiver = id
	msgs := sm.getMessages(id)
	sm.uiHandler.NewScreen(msgs)
	sm.refreshSelectedChatHistory(id)
	limit := sm.uiHandler.GetViewportLines()
	if limit < 50 {
		limit = 50
	}
	tail := msgs
	if len(tail) > limit {
		tail = tail[len(tail)-limit:]
	}
	sm.translateScreenMessages(tail)
	sm.transcribeScreenMessages(tail)

	sm.scheduleAutoMarkRead(id, intent)
}

// refreshSelectedChatHistory asks the phone for a recent authoritative slice
// without delaying the cached screen. Reactions are mutable message metadata,
// so a local message cache alone cannot be the final authority. The throttle
// prevents rapid sidebar navigation from flooding the linked-device channel.
func (sm *SessionManager) refreshSelectedChatHistory(chatID string) {
	now := time.Now()
	sm.historyRefreshMu.Lock()
	last := sm.historyRefreshAt[chatID]
	if !last.IsZero() && now.Sub(last) < 30*time.Second {
		sm.historyRefreshMu.Unlock()
		return
	}
	sm.historyRefreshAt[chatID] = now
	sm.historyRefreshMu.Unlock()

	go func() {
		started := time.Now()
		err := sm.requestChatHistorySync(chatID)
		if err != nil {
			fmt.Fprintf(os.Stdout, "[history-refresh] request failed error=%q duration_ms=%d\n", err, time.Since(started).Milliseconds())
			return
		}
		fmt.Fprintf(os.Stdout, "[history-refresh] request sent duration_ms=%d\n", time.Since(started).Milliseconds())
	}()
}

// scheduleAutoMarkRead arranges for `chatID` to be marked as read.
//
// On COMMIT (e.g. Enter on a chat row, /read, an explicit "I want this chat"
// signal), the read receipt fires immediately.
//
// On PROBE (e.g. Up/Down arrow navigation through the chat list), we instead
// start a short timer. If the user moves on to another chat before the timer
// fires, the timer is cancelled and the previous chat stays unread. This
// prevents arrowing past a queue of unread chats from blowing them all away.
//
// The state is guarded by markReadMu so concurrent selects from the network
// goroutine and timer fires from the timer goroutine don't race.
func (sm *SessionManager) scheduleAutoMarkRead(chatID string, intent SelectIntent) {
	sm.markReadMu.Lock()
	if sm.markReadTimer != nil {
		sm.markReadTimer.Stop()
		sm.markReadTimer = nil
		sm.markReadTimerChat = ""
	}

	if intent == SelectIntentCommit {
		fn := sm.markReadFn
		sm.markReadMu.Unlock()
		go fn(chatID)
		return
	}

	delay := sm.markReadDelay
	if delay <= 0 {
		// Treat a zero/negative delay as "fire immediately". Useful for tests.
		fn := sm.markReadFn
		sm.markReadMu.Unlock()
		go fn(chatID)
		return
	}

	sm.markReadTimerChat = chatID
	sm.markReadTimer = time.AfterFunc(delay, func() {
		sm.markReadMu.Lock()
		// Verify the user is STILL on this chat. Two ways this can fail:
		//   1. setCurrentReceiver fired again before AfterFunc started, but
		//      the Stop() lost the race - we'd see markReadTimerChat point
		//      to a different chat (or be empty).
		//   2. The user disconnected/reset.
		// In either case we abort silently.
		if sm.currentReceiver != chatID || sm.markReadTimerChat != chatID {
			sm.markReadMu.Unlock()
			return
		}
		sm.markReadTimer = nil
		sm.markReadTimerChat = ""
		fn := sm.markReadFn
		sm.markReadMu.Unlock()
		fn(chatID)
	})
	sm.markReadMu.Unlock()
}

// cancelPendingAutoMarkRead drops any in-flight PROBE timer without firing.
// Called when the user explicitly marks a chat unread, so a stale timer
// can't immediately undo the user's action by clearing the unread state
// they just set.
func (sm *SessionManager) cancelPendingAutoMarkRead() {
	sm.markReadMu.Lock()
	defer sm.markReadMu.Unlock()
	if sm.markReadTimer != nil {
		sm.markReadTimer.Stop()
		sm.markReadTimer = nil
		sm.markReadTimerChat = ""
	}
}

// hasPendingMarkRead reports whether chatID is still inside its PROBE dwell
// window (auto-mark-read timer armed but not yet fired).
func (sm *SessionManager) hasPendingMarkRead(chatID string) bool {
	sm.markReadMu.Lock()
	defer sm.markReadMu.Unlock()
	return sm.markReadTimer != nil && sm.markReadTimerChat == chatID
}

// sendReadReceipt tells WhatsApp — and thereby the phone — that one incoming
// message has been read. Needed for messages that arrive while their chat is
// already open: they never get a local unread flag, so autoMarkRead's batch
// path never sees them, and without a receipt the phone keeps showing a
// badge for a conversation the user is actively looking at.
func (sm *SessionManager) sendReadReceipt(msg Message) {
	if sm.client == nil || !sm.client.IsConnected() {
		return
	}
	chatJID, err := types.ParseJID(msg.ChatId)
	if err != nil {
		return
	}
	sender := chatJID
	if strings.Contains(msg.ChatId, GROUPSUFFIX) && msg.SenderId != "" {
		if s, e := types.ParseJID(msg.SenderId); e == nil {
			sender = s
		}
	}
	_ = sm.client.MarkRead(context.Background(), []types.MessageID{types.MessageID(msg.Id)}, time.Now(), chatJID, sender)
}

// autoMarkRead marks the current chat as read both locally and on WhatsApp,
// mirroring the phone behaviour of clearing unread on opening a conversation.
func (sm *SessionManager) autoMarkRead(chatID string) {
	unread := sm.db.MarkChatRead(chatID)
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.db.SaveChatCache()

	if len(unread) == 0 || sm.client == nil || !sm.client.IsConnected() {
		return
	}

	chatJID, err := types.ParseJID(chatID)
	if err != nil {
		return
	}

	type senderBatch struct {
		sender    types.JID
		ids       []types.MessageID
		timestamp time.Time
	}
	batches := make(map[string]*senderBatch)
	for _, msg := range unread {
		sender := chatJID
		if strings.Contains(chatID, GROUPSUFFIX) && msg.SenderId != "" {
			if s, e := types.ParseJID(msg.SenderId); e == nil {
				sender = s
			}
		}
		key := sender.String()
		if _, ok := batches[key]; !ok {
			batches[key] = &senderBatch{sender: sender}
		}
		batches[key].ids = append(batches[key].ids, types.MessageID(msg.Id))
		ts := time.Unix(int64(msg.Timestamp), 0)
		if ts.After(batches[key].timestamp) {
			batches[key].timestamp = ts
		}
	}
	for _, batch := range batches {
		if batch.timestamp.IsZero() {
			batch.timestamp = time.Now()
		}
		_ = sm.client.MarkRead(context.Background(), batch.ids, batch.timestamp, chatJID, batch.sender)
	}
}

// TranscribeCurrentChat triggers transcription for audio messages in the
// currently viewed chat. Called after the transcription model finishes loading.
func (sm *SessionManager) TranscribeCurrentChat() {
	if sm.currentReceiver == "" {
		return
	}
	msgs := sm.getMessages(sm.currentReceiver)
	limit := sm.uiHandler.GetViewportLines()
	if limit < 50 {
		limit = 50
	}
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	sm.transcribeScreenMessages(msgs)
}

func (sm *SessionManager) getConnection() (*whatsmeow.Client, error) {
	if sm.client == nil {
		dbPath := config.GetSessionFilePath() + ".db"
		container, err := sqlstore.New(context.Background(), "sqlite3", "file:"+dbPath+"?_foreign_keys=on", waLog.Noop)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to database: %v", err)
		}
		deviceStore, err := container.GetFirstDevice(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get device: %v", err)
		}
		// Set WHATSCLI_LOG=1 to get whatsmeow debug logs on stderr; the app
		// supervisor discards them, so this only matters when the server is
		// run by hand to diagnose sync issues.
		clientLog := waLog.Noop
		if os.Getenv("WHATSCLI_LOG") != "" {
			clientLog = waLog.Stdout("Client", "DEBUG", false)
		}
		client := whatsmeow.NewClient(deviceStore, clientLog)
		client.AddEventHandler(sm.eventHandler.Handle)
		sm.client = client
		sm.container = container
	}
	return sm.client, nil
}

func (sm *SessionManager) login() error {
	sm.client = nil
	client, err := sm.getConnection()
	if err != nil {
		return fmt.Errorf("failed to create WhatsApp connection: %v", err)
	}
	return sm.loginWithConnection(client)
}

func (sm *SessionManager) loginWithConnection(client *whatsmeow.Client) error {
	sm.uiHandler.PrintText("connecting..")
	if client.IsConnected() {
		client.Disconnect()
		sm.StatusChannel <- StatusMsg{false, nil}
		time.Sleep(500 * time.Millisecond)
	}

	if client.Store.ID == nil {
		return sm.loginWithQRCode(client)
	}

	if err := client.Connect(); err != nil {
		if errors.Is(err, whatsmeow.ErrNotConnected) || errors.Is(err, whatsmeow.ErrNotLoggedIn) {
			sm.uiHandler.PrintText("Session expired, need to scan QR code again")
			if delErr := client.Store.Delete(context.Background()); delErr != nil {
				return fmt.Errorf("failed to clear expired session: %v", delErr)
			}
			sm.client = nil
			client, err = sm.getConnection()
			if err != nil {
				return fmt.Errorf("failed to create new connection: %v", err)
			}
			return sm.loginWithQRCode(client)
		}
		return fmt.Errorf("connection failed: %v", err)
	}

	sm.uiHandler.PrintText("Session restored successfully")
	sm.StatusChannel <- StatusMsg{true, nil}
	go sm.loadRecentChats()
	return nil
}

func (sm *SessionManager) loginWithQRCode(client *whatsmeow.Client) error {
	sm.uiHandler.PrintText("Please scan the QR code with your phone")
	qrChan, err := client.GetQRChannel(context.Background())
	if err != nil {
		return fmt.Errorf("failed to initialize QR channel: %v", err)
	}
	if err = client.Connect(); err != nil {
		return fmt.Errorf("error connecting to WhatsApp: %v", err)
	}

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			sm.uiHandler.QRCode(evt.Code)
		case "success":
			sm.uiHandler.LoginSuccess()
			sm.StatusChannel <- StatusMsg{true, nil}
			go sm.loadRecentChats()
			return nil
		default:
			sm.uiHandler.QREvent(evt.Event)
		}
	}
	return errors.New("QR code channel closed without success")
}

func (sm *SessionManager) loadRecentChats() {
	debugLoad := os.Getenv("WHATSCLI_UNREAD_PROBE") != ""
	trace := func(step string) {
		if debugLoad {
			fmt.Fprintf(os.Stdout, "[load-trace] %s\n", step)
		}
	}
	trace("start")
	// Connect() returns before the handshake finishes, so give the
	// connection a moment instead of bailing — losing this race used to
	// leave the session up but the chat list never loaded.
	for i := 0; i < 40 && (sm.client == nil || !sm.client.IsConnected()); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if sm.client == nil || !sm.client.IsConnected() {
		trace("gave up waiting for connection")
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}
	trace("connected")

	sm.db.LoadChatCache()
	sm.db.LoadMessageCache()
	trace("caches loaded")
	// Unread flags older than WhatsApp's offline-receipt replay window can
	// never be reconciled with the phone again; drop them so long downtimes
	// don't leave permanent phantom badges.
	sm.db.PruneStaleUnread(time.Now().Add(-14 * 24 * time.Hour).Unix())
	sm.db.RecomputeUnreadCounts()
	sm.loadContacts()
	sm.db.RefreshContactNames()
	sm.db.SaveMessageCache()
	trace("contacts refreshed, message cache saved")

	groups, err := sm.client.GetJoinedGroups(context.Background())
	if err == nil {
		for _, group := range groups {
			sm.db.AddChat(Chat{
				Id:      group.JID.String(),
				IsGroup: true,
				Name:    group.Name,
			})
		}
	}
	trace("groups loaded")

	sm.syncChatSettings()
	trace("chat settings synced")
	sm.syncAppState()
	trace("app state synced")
	sm.uiHandler.SetChats(sm.db.GetChatIds())

	// Converge unread badges with the phone in the background; receipts
	// missed while we were offline are gone forever, so ask the phone
	// directly for its current per-chat state.
	go sm.reconcileUnreadWithPhone()

	// Diagnostic hook: WHATSCLI_UNREAD_PROBE=<jid>[,<jid>…] requests an
	// on-demand history sync for those chats after connect, to test whether
	// the phone reports unreadCount in the responses.
	if probe := os.Getenv("WHATSCLI_UNREAD_PROBE"); probe != "" {
		for _, chatID := range strings.Split(probe, ",") {
			chatID = strings.TrimSpace(chatID)
			if err := sm.requestChatHistorySync(chatID); err != nil {
				fmt.Fprintf(os.Stdout, "[unread-probe] request %s failed: %v\n", chatID, err)
			} else {
				fmt.Fprintf(os.Stdout, "[unread-probe] requested history sync for %s\n", chatID)
			}
		}
	}
}

// syncChatSettings reads archived/pinned state from whatsmeow's ChatSettingsStore
// and applies it to our in-memory chat list.
func (sm *SessionManager) syncChatSettings() {
	if sm.client == nil || sm.client.Store == nil || sm.client.Store.ChatSettings == nil {
		return
	}
	for _, chat := range sm.db.GetChatIds() {
		jid, err := types.ParseJID(chat.Id)
		if err != nil {
			continue
		}
		settings, err := sm.client.Store.ChatSettings.GetChatSettings(context.Background(), jid)
		if err != nil {
			continue
		}
		sm.db.SetChatArchived(chat.Id, settings.Archived)
		sm.db.SetChatPinned(chat.Id, settings.Pinned)
	}
}

// syncAppState fetches app state to get MarkChatAsRead events that tell us
// which chats have been read or marked unread on the phone.
//
// The first sync of each launch replays the FULL regular_low collection.
// Empirically the server compacts old mark-read entries away (the snapshot
// can be a few hundred bytes), so this is NOT a full historical
// reconciliation — its real value is (a) whatever recent read/unread actions
// the server still holds, and (b) resetting our local LTHash state, which
// otherwise drifts and makes our own SendAppState mark-unread patches bounce
// with 409/mismatching-LTHash. Reconnects within the same process only fetch
// incremental patches. Historical convergence comes from read-self receipts
// (live + offline replay) and PruneStaleUnread.
func (sm *SessionManager) syncAppState() {
	if sm.client == nil {
		return
	}
	sm.client.EmitAppStateEventsOnFullSync = true
	ctx := context.Background()
	// INCREMENTAL by default: the version cursor persists in the session DB,
	// so this replays every patch the phone wrote while we were offline —
	// including markChatAsRead. (A full resync would DELETE that cursor and
	// re-seed from the server's compacted snapshot, silently discarding the
	// offline patches, so full sync is reserved for corruption repair.)
	err := sm.client.FetchAppState(ctx, appstate.WAPatchRegularLow, false, false)
	if err != nil {
		// Mismatching-LTHash means our local app state is corrupted; reset
		// it with a full resync (also what fixes SendAppState 409s).
		err = sm.client.FetchAppState(ctx, appstate.WAPatchRegularLow, true, false)
	}
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("read-state sync with phone failed: %v", err))
	}
}

// reconcileUnreadWithPhone asks the phone for the authoritative unread state
// of the most recently active chats, once per launch. Read receipts only
// reach us while we're running (WhatsApp's offline replay is bounded and
// lossy for busy groups), so after downtime our local unread flags drift
// from the phone. On-demand history sync responses carry the phone's own
// unreadCount per conversation; handleHistorySync applies them. Runs in a
// goroutine: one peer message per chat, spaced out to be polite.
func (sm *SessionManager) reconcileUnreadWithPhone() {
	const maxChats = 30
	started := time.Now()
	fmt.Fprintf(os.Stdout, "[unread-sync] started max_chats=%d\n", maxChats)
	requested := 0
	for _, chat := range sm.db.GetChatIds() {
		if requested >= maxChats {
			break
		}
		if err := sm.requestChatHistorySync(chat.Id); err != nil {
			continue // no anchor message or transient send failure — skip
		}
		requested++
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Fprintf(os.Stdout, "[unread-sync] requests completed sent=%d duration_ms=%d\n", requested, time.Since(started).Milliseconds())
}

// requestChatHistorySync asks the primary phone for the most recent history
// of one chat via an on-demand history sync peer message. The response
// arrives as an events.HistorySync (type ON_DEMAND) and flows through
// handleHistorySync, which applies the phone's own unreadCount — the only
// authoritative source of a chat's read state.
func (sm *SessionManager) requestChatHistorySync(chatID string) error {
	if sm.client == nil || !sm.client.IsConnected() {
		return errors.New("not connected")
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	// The phone indexes migrated 1:1 chats by LID; a request addressed by
	// phone-number JID gets silently ignored for those, so translate first.
	if jid.Server == types.DefaultUserServer && sm.client.Store != nil && sm.client.Store.LIDs != nil {
		if lid, lidErr := sm.client.Store.LIDs.GetLIDForPN(context.Background(), jid); lidErr == nil && !lid.IsEmpty() {
			jid = lid
		}
	}
	msgs := sm.db.GetMessages(chatID)
	if len(msgs) == 0 {
		return errors.New("no anchor message for " + chatID)
	}
	last := msgs[len(msgs)-1]
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, IsFromMe: last.FromMe},
		ID:            last.Id,
		Timestamp:     time.Unix(int64(last.Timestamp), 0),
	}
	req := sm.client.BuildHistorySyncRequest(info, 50)
	_, err = sm.client.SendPeerMessage(context.Background(), req)
	return err
}

func (sm *SessionManager) loadContacts() {
	if sm.client == nil || sm.client.Store == nil || sm.client.Store.Contacts == nil {
		return
	}

	contacts, err := sm.client.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to load contacts: %v", err))
		return
	}

	contactsByPN := make(map[string]Contact, len(contacts))
	contactCount := 0
	for jid, contact := range contacts {
		name := contact.FullName
		if name == "" {
			name = contact.PushName
		}
		if name == "" {
			name = jid.User
		}
		short := contact.PushName
		if short == "" {
			short = name
		}
		c := Contact{
			Id:    jid.String(),
			Name:  name,
			Short: short,
		}
		sm.db.AddContact(c)
		contactsByPN[jid.User] = c
		contactCount++
	}

	if sm.client.Store.LIDs != nil {
		dbPath := config.GetSessionFilePath() + ".db"
		if db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on&mode=ro"); err == nil {
			defer db.Close()
			rows, err := db.Query("SELECT lid, pn FROM whatsmeow_lid_map")
			if err == nil {
				for rows.Next() {
					var lid, pn string
					if rows.Scan(&lid, &pn) == nil {
						if c, ok := contactsByPN[pn]; ok {
							sm.db.AddContact(Contact{
								Id:    lid + "@lid",
								Name:  c.Name,
								Short: c.Short,
							})
						}
					}
				}
				rows.Close()
			}
		}
	}

	if contactCount > 0 {
		sm.uiHandler.PrintText(fmt.Sprintf("Loaded %d contacts", contactCount))
	}
}

func (sm *SessionManager) getChatName(jid types.JID) string {
	if jid.Server == types.GroupServer {
		groupInfo, err := sm.client.GetGroupInfo(context.Background(), jid)
		if err == nil && groupInfo.Name != "" {
			return groupInfo.Name
		}
	}
	if sm.client != nil && sm.client.Store != nil && sm.client.Store.Contacts != nil {
		contact, err := sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return sm.db.GetIdName(jid.String())
}

func (sm *SessionManager) disconnect() error {
	sm.userDisconnected = true
	if sm.client != nil && sm.client.IsConnected() {
		sm.client.Disconnect()
		sm.StatusChannel <- StatusMsg{false, nil}
	}
	return nil
}

func (sm *SessionManager) logout() error {
	if sm.client == nil {
		sm.StatusChannel <- StatusMsg{false, nil}
		sm.uiHandler.PrintText("Already logged out")
		return nil
	}

	if sm.client.Store != nil && sm.client.Store.ID != nil {
		if err := sm.client.Logout(context.Background()); err != nil && !errors.Is(err, whatsmeow.ErrNotConnected) {
			sm.uiHandler.PrintText("Warning: Couldn't fully log out: " + err.Error())
		}
	}
	sm.client = nil
	sm.container = nil
	sm.StatusChannel <- StatusMsg{false, nil}
	sm.uiHandler.PrintText("Successfully logged out")
	return nil
}

func (sm *SessionManager) execCommand(command Command) {
	switch command.Name {
	default:
		sm.uiHandler.PrintText("[" + config.Config.Colors.Negative + "]Unknown command: [-]" + command.Name)
	case "backlog":
		sm.loadBacklog()
	case "login", "connect":
		sm.userDisconnected = false
		err := sm.login()
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("WhatsApp connection failed: %v", err))
			sm.uiHandler.PrintText("Try using /reset to completely reset the connection")
		} else {
			sm.uiHandler.PrintText("Successfully connected to WhatsApp")
		}
	case "reset":
		sm.resetSession()
	case "disconnect":
		sm.uiHandler.PrintError(sm.disconnect())
	case "logout":
		sm.uiHandler.PrintError(sm.logout())
	case "send":
		if checkParam(command.Params, 2) {
			sm.sendText(command.Params[0], strings.Join(command.Params[1:], " "))
		} else {
			sm.printCommandUsage("send", "[chat-id[] [message text[]")
		}
	case "select":
		if checkParam(command.Params, 1) {
			id := command.Params[0]
			intent := command.Intent
			for {
				select {
				case next := <-sm.CommandChannel:
					if next.Name == "select" && checkParam(next.Params, 1) {
						id = next.Params[0]
						// COMMIT trumps PROBE: if anywhere in the
						// coalesced burst the user pressed Enter, the
						// final selection is a commit.
						if next.Intent == SelectIntentCommit {
							intent = SelectIntentCommit
						}
						continue
					}
					sm.setCurrentReceiver(id, intent)
					sm.execCommand(next)
					id = ""
				default:
				}
				break
			}
			if id != "" {
				sm.setCurrentReceiver(id, intent)
			}
		} else {
			sm.printCommandUsage("select", "[chat-id[]")
		}
	case "read":
		sm.markCurrentChatRead()
	case "unread":
		sm.markCurrentChatUnread()
	case "info":
		if checkParam(command.Params, 1) {
			sm.uiHandler.PrintText(sm.db.GetMessageInfo(command.Params[0]))
		} else {
			sm.printCommandUsage("info", "[message-id[]")
		}
	case "download":
		sm.downloadCommand(command.Params, false, false)
	case "open":
		sm.downloadCommand(command.Params, true, false)
	case "show":
		sm.downloadCommand(command.Params, true, true)
	case "url":
		sm.openMessageURL(command.Params)
	case "upload":
		sm.sendMediaCommand(command.Params, MessageKindDocument)
	case "sendimage":
		sm.sendMediaCommand(command.Params, MessageKindImage)
	case "sendvideo":
		sm.sendMediaCommand(command.Params, MessageKindVideo)
	case "sendaudio":
		sm.sendMediaCommand(command.Params, MessageKindAudio)
	case "revoke":
		sm.revokeMessage(command.Params)
	case "forcetranslate":
		// User-driven re-translate of a single message. Bypasses the
		// classifier and any cached result so it always produces a
		// fresh translation; we don't want our heuristics to trap
		// users in "this is English" when it isn't.
		if !checkParam(command.Params, 1) {
			sm.printCommandUsage("forcetranslate", "[message-id[]")
			break
		}
		msg, ok := sm.db.GetMessage(command.Params[0])
		if !ok {
			sm.uiHandler.PrintError(fmt.Errorf("message not found: %s", command.Params[0]))
			break
		}
		sm.translateMessageForce(msg)
	case "leave":
		sm.leaveCurrentGroup()
	case "create":
		sm.createGroup(command.Params)
	case "add":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeAdd, "add", "added new members")
	case "remove":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeRemove, "remove", "removed members")
	case "admin":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangePromote, "admin", "promoted members")
	case "removeadmin":
		sm.updateCurrentGroupParticipants(command.Params, whatsmeow.ParticipantChangeDemote, "removeadmin", "demoted members")
	case "subject":
		sm.updateCurrentGroupSubject(command.Params)
	case "colorlist":
		sm.uiHandler.PrintText("Color list is handled by the frontend")
	case "more":
		sm.loadBacklog()
	}
}

func (sm *SessionManager) loadBacklog() {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("backlog", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	jid, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	existingMessages := sm.db.GetMessages(sm.currentReceiver)
	oldest, ok := sm.db.GetOldestMessage(sm.currentReceiver)
	if !ok {
		if !sm.requestHistoryWithDBAnchors(jid) {
			sm.uiHandler.PrintText("No message anchors found for this chat. New messages will appear when they arrive.")
			return
		}
		sm.uiHandler.PrintText("Requested message history from WhatsApp. Messages should appear shortly...")
		deadline := time.Now().Add(15 * time.Second)
		for len(sm.db.GetMessages(sm.currentReceiver)) == 0 && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		updated := sm.db.GetMessages(sm.currentReceiver)
		if len(updated) > 0 {
			sm.uiHandler.PrintText(fmt.Sprintf("Loaded %d messages.", len(updated)))
			sm.uiHandler.NewScreen(updated)
		} else {
			sm.uiHandler.PrintText("History request sent. Messages may take a moment to arrive. Try pressing " + config.Config.Keymap.CommandBacklog + " again in a few seconds.")
		}
		return
	}

	sm.uiHandler.PrintText("Retrieving message history...")
	senderJID := types.EmptyJID
	if oldest.SenderId != "" {
		if parsedSender, parseErr := types.ParseJID(oldest.SenderId); parseErr == nil {
			senderJID = parsedSender
		}
	}
	req := sm.client.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     jid,
			Sender:   senderJID,
			IsFromMe: oldest.FromMe,
			IsGroup:  strings.Contains(sm.currentReceiver, GROUPSUFFIX),
		},
		ID:        types.MessageID(oldest.Id),
		Timestamp: time.Unix(int64(oldest.Timestamp), 0),
	}, config.Config.General.BacklogMsgQuantity)
	if _, err = sm.client.SendPeerMessage(context.Background(), req); err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to request message history: %v", err))
		sm.uiHandler.NewScreen(existingMessages)
		return
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(sm.db.GetMessages(sm.currentReceiver)) == len(existingMessages) && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}

	updated := sm.db.GetMessages(sm.currentReceiver)
	if len(updated) > len(existingMessages) {
		sm.uiHandler.PrintText(fmt.Sprintf("Loaded %d additional messages", len(updated)-len(existingMessages)))
	} else {
		sm.uiHandler.PrintText("No additional messages found. WhatsApp may limit history access.")
	}
	sm.uiHandler.NewScreen(updated)
}

func (sm *SessionManager) requestHistoryWithDBAnchors(chatJID types.JID) bool {
	dbPath := config.GetSessionFilePath() + ".db"
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on&mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()

	var senderJIDStr, messageID string
	err = db.QueryRow(
		"SELECT sender_jid, message_id FROM whatsmeow_message_secrets WHERE chat_jid = ? ORDER BY rowid DESC LIMIT 1",
		chatJID.String(),
	).Scan(&senderJIDStr, &messageID)
	if err != nil || messageID == "" {
		return false
	}

	senderJID := types.EmptyJID
	if senderJIDStr != "" {
		if parsed, parseErr := types.ParseJID(senderJIDStr); parseErr == nil {
			senderJID = parsed
		}
	}

	isGroup := strings.Contains(chatJID.String(), GROUPSUFFIX)
	isFromMe := false
	if sm.client.Store.ID != nil {
		isFromMe = senderJID.User == sm.client.Store.ID.User
	}

	count := config.Config.General.BacklogMsgQuantity
	if count < 50 {
		count = 50
	}
	req := sm.client.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chatJID,
			Sender:   senderJID,
			IsFromMe: isFromMe,
			IsGroup:  isGroup,
		},
		ID:        types.MessageID(messageID),
		Timestamp: time.Now(),
	}, count)
	_, err = sm.client.SendPeerMessage(context.Background(), req)
	return err == nil
}

func (sm *SessionManager) resetSession() {
	if sm.client != nil {
		if sm.client.IsConnected() {
			sm.client.Disconnect()
		}
		if sm.client.Store != nil {
			if err := sm.client.Store.Delete(context.Background()); err != nil {
				sm.uiHandler.PrintText("Warning: Couldn't remove session: " + err.Error())
			}
		}
	}

	sm.client = nil
	sm.container = nil
	dbPath := config.GetSessionFilePath() + ".db"
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		sm.uiHandler.PrintText("Warning: Couldn't remove database file: " + err.Error())
	}
	sm.StatusChannel <- StatusMsg{false, nil}
	sm.uiHandler.PrintText("Session reset. Use /connect to reconnect with a new QR code.")
}

func (sm *SessionManager) markCurrentChatRead() {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("read", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	chatJID, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	unreadMessages := sm.db.MarkChatRead(sm.currentReceiver)
	if len(unreadMessages) == 0 {
		sm.uiHandler.SetChats(sm.db.GetChatIds())
		sm.uiHandler.PrintText("No unread messages in current chat")
		return
	}

	type senderBatch struct {
		sender    types.JID
		ids       []types.MessageID
		timestamp time.Time
	}
	batches := make(map[string]*senderBatch)
	for _, msg := range unreadMessages {
		sender := chatJID
		if strings.Contains(sm.currentReceiver, GROUPSUFFIX) && msg.SenderId != "" {
			sender, err = types.ParseJID(msg.SenderId)
			if err != nil {
				continue
			}
		}
		key := sender.String()
		if _, ok := batches[key]; !ok {
			batches[key] = &senderBatch{sender: sender}
		}
		batches[key].ids = append(batches[key].ids, types.MessageID(msg.Id))
		ts := time.Unix(int64(msg.Timestamp), 0)
		if ts.After(batches[key].timestamp) {
			batches[key].timestamp = ts
		}
	}

	for _, batch := range batches {
		if batch.timestamp.IsZero() {
			batch.timestamp = time.Now()
		}
		if err := sm.client.MarkRead(context.Background(), batch.ids, batch.timestamp, chatJID, batch.sender); err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("failed to mark messages as read: %v", err))
		}
	}

	sm.uiHandler.SetChats(sm.db.GetChatIds())
}

func (sm *SessionManager) markCurrentChatUnread() {
	if sm.currentReceiver == "" {
		sm.printCommandUsage("unread", "-> only works in a chat")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	chatJID, err := types.ParseJID(sm.currentReceiver)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	// A PROBE timer scheduled by the very selection that put us on this
	// chat could otherwise fire moments after we mark unread, instantly
	// undoing the user's action. Cancel it.
	sm.cancelPendingAutoMarkRead()

	sm.db.SetChatUnreadCount(sm.currentReceiver, 1)

	patch := appstate.BuildMarkChatAsRead(chatJID, false, time.Now(), nil)
	err = sm.client.SendAppState(context.Background(), patch)
	if err != nil {
		// Our local app-state snapshot can drift from the server's (surfacing
		// as a 409 conflict / "mismatching LTHash"). Force a full resync of
		// the collection and retry once before giving up.
		if resyncErr := sm.client.FetchAppState(context.Background(), appstate.WAPatchRegularLow, true, false); resyncErr == nil {
			err = sm.client.SendAppState(context.Background(), patch)
		}
	}
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to mark chat as unread: %v", err))
	}

	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.db.SaveChatCache()
}

// GetAvatar returns the profile picture for a chat (contact photo or group
// icon), fetching it from WhatsApp on first use and caching it — including
// misses — for the session.
func (sm *SessionManager) GetAvatar(chatID string, preview bool) ([]byte, string, error) {
	key := chatID
	if preview {
		key += "|preview"
	}
	sm.avatarMu.Lock()
	if sm.avatarCache == nil {
		sm.avatarCache = make(map[string]avatarEntry)
	}
	if e, ok := sm.avatarCache[key]; ok {
		sm.avatarMu.Unlock()
		return e.data, e.mime, e.err
	}
	sm.avatarMu.Unlock()

	data, mimeType, err := sm.fetchAvatar(chatID, preview)

	sm.avatarMu.Lock()
	sm.avatarCache[key] = avatarEntry{data: data, mime: mimeType, err: err}
	sm.avatarMu.Unlock()
	return data, mimeType, err
}

func (sm *SessionManager) fetchAvatar(chatID string, preview bool) ([]byte, string, error) {
	if sm.client == nil || !sm.client.IsConnected() {
		return nil, "", errors.New("not connected to WhatsApp")
	}
	jid, err := types.ParseJID(chatID)
	if err != nil {
		return nil, "", fmt.Errorf("invalid JID: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := sm.client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{Preview: preview})
	if err != nil {
		if errors.Is(err, whatsmeow.ErrProfilePictureNotSet) || errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized) {
			return nil, "", ErrNoAvatar
		}
		return nil, "", err
	}
	if info == nil || info.URL == "" {
		return nil, "", ErrNoAvatar
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("avatar download failed: %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "image/jpeg"
	}
	return data, mimeType, nil
}

func (sm *SessionManager) downloadCommand(params []string, preview, show bool) {
	if !checkParam(params, 1) {
		name := "download"
		if preview && !show {
			name = "open"
		} else if show {
			name = "show"
		}
		sm.printCommandUsage(name, "[message-id[]")
		return
	}

	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}

	path, err := sm.downloadMessage(msg, preview || show)
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}

	// Always notify the structured FileSaved hook so the gRPC TUI can
	// pin the "→ saved to …" annotation under the originating message
	// in the chat view. The tview legacy UI no-ops on this; it still
	// gets the human-readable line via PrintText below for "download"
	// and the OpenFile call for "open"/"show".
	sm.uiHandler.FileSaved(msg.Id, path)

	if show || preview {
		sm.uiHandler.OpenFile(path)
		return
	}
	sm.uiHandler.PrintText("[::d] -> " + path + "[::-]")
}

func (sm *SessionManager) openMessageURL(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("url", "[message-id[]")
		return
	}
	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}
	url := urlPattern.FindString(msg.Text)
	if url == "" {
		sm.uiHandler.PrintText("No URL found in message")
		return
	}
	sm.uiHandler.OpenFile(url)
}

func (sm *SessionManager) sendMediaCommand(params []string, kind MessageKind) {
	if sm.currentReceiver == "" {
		sm.printCommandUsage(commandNameForKind(kind), "-> only works in a chat")
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage(commandNameForKind(kind), "/path/to/file")
		return
	}
	path := strings.Join(params, " ")
	sm.uiHandler.PrintError(sm.sendMedia(sm.currentReceiver, path, kind))
}

func (sm *SessionManager) revokeMessage(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("revoke", "[message-id[]")
		return
	}
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	msg, ok := sm.db.GetMessage(params[0])
	if !ok {
		sm.uiHandler.PrintError(errors.New("message not found"))
		return
	}
	chatJID, err := types.ParseJID(msg.ChatId)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid chat JID: %v", err))
		return
	}
	if _, err = sm.client.RevokeMessage(context.Background(), chatJID, types.MessageID(msg.Id)); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.db.MarkMessageRevoked(msg.Id)
	if sm.currentReceiver == msg.ChatId {
		sm.uiHandler.NewScreen(sm.getMessages(msg.ChatId))
	}
	sm.uiHandler.PrintText("revoked: " + msg.Id)
}

func (sm *SessionManager) leaveCurrentGroup() {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if err = sm.client.LeaveGroup(context.Background(), groupJID); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.uiHandler.PrintText("left group " + groupJID.String())
}

func (sm *SessionManager) createGroup(params []string) {
	if !checkParam(params, 1) {
		sm.printCommandUsage("create", "[user-id[] [user-id[] New Group Subject")
		sm.printCommandUsage("create", "New Group Subject")
		return
	}

	participants := make([]types.JID, 0)
	idx := 0
	for idx < len(params) && strings.Contains(params[idx], CONTACTSUFFIX) {
		participant, err := types.ParseJID(params[idx])
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("invalid user id %q: %v", params[idx], err))
			return
		}
		participants = append(participants, participant)
		idx++
	}

	name := strings.Join(params[idx:], " ")
	if name == "" {
		name = strings.Join(params, " ")
		participants = nil
	}

	groupInfo, err := sm.client.CreateGroup(context.Background(), whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: participants,
	})
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}

	sm.db.AddChat(Chat{
		Id:          groupInfo.JID.String(),
		IsGroup:     true,
		Name:        groupInfo.Name,
		LastMessage: time.Now().Unix(),
	})
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.uiHandler.PrintText("created new group " + groupInfo.JID.String())
}

func (sm *SessionManager) updateCurrentGroupParticipants(params []string, action whatsmeow.ParticipantChange, command, success string) {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage(command, "[user-id[]")
		return
	}

	participants := make([]types.JID, 0, len(params))
	for _, raw := range params {
		jid, err := types.ParseJID(raw)
		if err != nil {
			sm.uiHandler.PrintError(fmt.Errorf("invalid user id %q: %v", raw, err))
			return
		}
		participants = append(participants, jid)
	}

	if _, err = sm.client.UpdateGroupParticipants(context.Background(), groupJID, participants, action); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	sm.uiHandler.PrintText(success + " for " + groupJID.String())
}

func (sm *SessionManager) updateCurrentGroupSubject(params []string) {
	groupJID, err := sm.currentGroupJID()
	if err != nil {
		sm.uiHandler.PrintError(err)
		return
	}
	if !checkParam(params, 1) {
		sm.printCommandUsage("subject", "new-subject -> in group chat")
		return
	}

	name := strings.Join(params, " ")
	if err = sm.client.SetGroupName(context.Background(), groupJID, name); err != nil {
		sm.uiHandler.PrintError(err)
		return
	}

	sm.db.AddChat(Chat{
		Id:      groupJID.String(),
		IsGroup: true,
		Name:    name,
	})
	sm.uiHandler.SetChats(sm.db.GetChatIds())
	sm.uiHandler.PrintText("updated subject for " + groupJID.String())
}

func (sm *SessionManager) currentGroupJID() (types.JID, error) {
	if sm.currentReceiver == "" || !strings.Contains(sm.currentReceiver, GROUPSUFFIX) {
		return types.JID{}, errors.New("not a group")
	}
	return types.ParseJID(sm.currentReceiver)
}

func (sm *SessionManager) printCommandUsage(command, usage string) {
	sm.uiHandler.PrintText("[" + config.Config.Colors.Negative + "]Usage:[-] " + command + " " + usage)
}

func checkParam(arr []string, length int) bool {
	return arr != nil && len(arr) >= length
}

func (sm *SessionManager) getMessages(wid string) []Message {
	return sm.db.GetMessages(wid)
}

// GetChats returns the current chat list sorted by most recent message.
func (sm *SessionManager) GetChats() []Chat {
	return sm.db.GetChatIds()
}

// GetStatus returns the current session status.
func (sm *SessionManager) GetStatus() SessionStatus {
	return sm.statusInfo
}

func (sm *SessionManager) sendText(wid, text string) {
	if sm.client == nil || !sm.client.IsConnected() {
		sm.uiHandler.PrintError(errors.New("not connected to WhatsApp"))
		return
	}

	receiver, err := types.ParseJID(wid)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("invalid JID: %v", err))
		return
	}

	actualText := text
	translated := false
	if sm.shouldAutoTranslateOutgoing(wid) {
		threadLang, _ := sm.detectThreadLanguage(wid)
		targetCode := translate.ISOToDialectCode(threadLang, sm.Translator.Dialect())
		if result, err := sm.Translator.TranslateFromEnglish(text, targetCode); err == nil && result != "" {
			actualText = result
			translated = true
		}
	}

	raw := &waProto.Message{Conversation: proto.String(actualText)}
	sm.lastSent = time.Now()
	resp, err := sm.client.SendMessage(context.Background(), receiver, raw)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("failed to send message: %v", err))
		return
	}

	newMsg := sm.outgoingMessageFromSendResponse(resp, wid, raw, MessageKindText, actualText, "", "")
	sm.db.AddMessage(newMsg, false)
	if sm.currentReceiver == wid {
		sm.uiHandler.NewMessage(newMsg)
	}
	sm.uiHandler.SetChats(sm.db.GetChatIds())

	// Persist on send. Without this the message lives only in RAM, so a
	// backend restart loses every outbound message that was never echoed
	// back through the inbound event stream — and WhatsApp does not
	// re-deliver self-sent messages once they've been ack'd, so the
	// next history sync won't restore them either. Mirrors the
	// SaveChatCache/SaveMessageCache calls in handleLiveMessage.
	sm.db.SaveChatCache()
	sm.db.SaveMessageCache()

	if translated && sm.Translator != nil && sm.Translator.IsReady() {
		go sm.translateIncoming(newMsg)
	}
}

func (sm *SessionManager) translateScreenMessages(msgs []Message) {
	if sm.Translator == nil || !sm.Translator.IsReady() {
		return
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if !m.FromMe && m.Text != "" {
			texts = append(texts, m.Text)
		}
	}
	threadLang, confident := translate.DetectThreadLanguage(texts)
	if !confident || threadLang == "en" {
		return
	}
	go func() {
		translated := false
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			if msg.Kind == MessageKindAudio {
				if st, ok := sm.db.GetTranscription(msg.Id); ok && st != "" {
					msg.Text = st
				} else {
					continue
				}
			} else if msg.Text == "" || msg.Kind != MessageKindText {
				continue
			}
			if _, ok := sm.db.GetTranslation(msg.Id); ok {
				sm.translateIncoming(msg)
				continue
			}
			if translate.IsEnglish(msg.Text) {
				continue
			}
			sm.translateIncoming(msg)
			translated = true
		}
		if translated {
			sm.db.SaveTranslationCache()
		}
	}()
}

func (sm *SessionManager) transcribeScreenMessages(msgs []Message) {
	if sm.Transcriber == nil || !sm.Transcriber.IsReady() {
		return
	}
	go func() {
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			if msg.Kind != MessageKindAudio {
				continue
			}
			sm.transcribeAndTranslate(msg)
		}
	}()
}

// downloadAudioData downloads the raw bytes of an audio message from WhatsApp.
func (sm *SessionManager) downloadAudioData(msg Message) ([]byte, error) {
	if sm.client == nil || !sm.client.IsConnected() {
		return nil, errors.New("not connected")
	}
	downloadable, err := downloadableFromMessage(msg)
	if err != nil {
		return nil, err
	}
	return sm.client.Download(context.Background(), downloadable)
}

// transcribeAndTranslate downloads an audio message, transcribes it with Whisper,
// then triggers translation if the transcript is non-English.
func (sm *SessionManager) transcribeAndTranslate(msg Message) {
	if sm.Transcriber == nil || !sm.Transcriber.IsReady() {
		return
	}
	if cached, ok := sm.db.GetTranscription(msg.Id); ok {
		if cached != "" {
			sm.uiHandler.NewTranscription(msg, cached)
			msg.Text = cached
			sm.translateIncoming(msg)
		}
		return
	}

	data, err := sm.downloadAudioData(msg)
	if err != nil {
		return
	}

	result, err := sm.Transcriber.TranscribeAudio(data)
	if err != nil {
		return
	}
	if result.Text == "" {
		return
	}

	sm.db.StoreTranscription(msg.Id, result.Text)
	sm.db.SaveTranscriptionCache()
	sm.uiHandler.NewTranscription(msg, result.Text)

	if sm.Translator != nil && sm.Translator.IsReady() && result.Language != "en" {
		msg.Text = result.Text
		sm.translateIncoming(msg)
		sm.db.SaveTranslationCache()
	}
}

// translateIncoming runs the per-message classifier from the translate
// package and, when it says we should translate, invokes the LLM. The
// classifier is the single source of truth for "do we translate this?";
// see translate.ClassifyMessage for the rules.
//
// Historically this function had its own gating logic: it asked the
// thread detector, vetoed everything if the thread looked English, and
// then short-circuited again on a per-message English check. That broke
// mixed-language threads where the counterpart was pre-translating to
// English on their phone and only occasionally dropping back to their
// native language - the very messages most worth translating got vetoed
// because the thread (composed mostly of their pre-translated English)
// looked English. The classifier in translate/detect.go handles all of
// that now with a softer thread prior plus per-message overrides.
func (sm *SessionManager) translateIncoming(msg Message) {
	if msg.Text == "" || sm.Translator == nil || !sm.Translator.IsReady() {
		return
	}
	if cached, ok := sm.db.GetTranslation(msg.Id); ok {
		if cached != "" {
			sm.uiHandler.NewTranslation(msg, cached)
		}
		return
	}

	threadLang, _ := sm.detectThreadLanguage(msg.ChatId)
	srcLang, decision := translate.ClassifyMessage(msg.Text, threadLang)
	if decision != translate.DecisionTranslate {
		return
	}
	if srcLang == "" {
		srcLang = threadLang
	}
	if srcLang == "" || srcLang == "en" {
		// Defensive: ClassifyMessage shouldn't return DecisionTranslate
		// without a usable source. If we ever do hit this, skip rather
		// than feed the LLM an English source for English-looking text.
		return
	}

	result, err := sm.Translator.TranslateToEnglish(msg.Text, srcLang)
	if err != nil {
		return
	}
	if result != "" {
		sm.db.StoreTranslation(msg.Id, result)
		sm.uiHandler.NewTranslation(msg, result)
	}
}

// translateMessageForce runs translation on `msg` unconditionally,
// bypassing the classifier and any cached result. Used by the manual
// "translate this anyway" shortcut: if our heuristics ever disagree
// with the user, they get the final word in one keystroke.
//
// The cached translation (if any) is wiped before the new one is stored
// so the user-forced result becomes the new source of truth - retrying
// would otherwise hit the cache and yield the (potentially wrong) old
// translation.
func (sm *SessionManager) translateMessageForce(msg Message) {
	if msg.Text == "" || sm.Translator == nil || !sm.Translator.IsReady() {
		return
	}
	threadLang, _ := sm.detectThreadLanguage(msg.ChatId)

	srcLang := translate.DetectLanguage(msg.Text)
	if srcLang == "" || srcLang == "en" {
		// Fall back to the thread language so a forced-translate on a
		// short or ambiguous message still has something to offer the
		// LLM. If even that's empty, default to the dialect base
		// (e.g. "es" from "es-CR") - the user explicitly asked us to
		// translate, so refusing would be the wrong default.
		if threadLang != "" && threadLang != "en" {
			srcLang = threadLang
		} else {
			srcLang = translate.BaseLanguageCode(sm.Translator.Dialect())
		}
	}
	if srcLang == "" {
		return
	}

	sm.db.DeleteTranslation(msg.Id)
	result, err := sm.Translator.TranslateToEnglish(msg.Text, srcLang)
	if err != nil {
		sm.uiHandler.PrintError(fmt.Errorf("translate: %v", err))
		return
	}
	if result == "" {
		return
	}
	sm.db.StoreTranslation(msg.Id, result)
	sm.db.SaveTranslationCache()
	sm.uiHandler.NewTranslation(msg, result)
}

func (sm *SessionManager) detectThreadLanguage(chatID string) (string, bool) {
	msgs := sm.db.GetMessages(chatID)
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if !m.FromMe && m.Text != "" {
			texts = append(texts, m.Text)
		}
	}
	return translate.DetectThreadLanguage(texts)
}

// shouldAutoTranslateOutgoing decides whether an outbound English message
// should be silently translated into the chat's target language and sent
// in that language *as the literal message body* rather than the user's
// original English.
//
// Outbound translation has a much higher cost-of-error than inbound:
//
//   - A wrong inbound translation is just a noisy hint we render below
//     the original; the user can ignore it and read the source.
//   - A wrong *outbound* translation actually goes out over the wire to
//     another human's phone. The recipient sees the wrong-language send;
//     the sender has to revoke and re-type. We can't auto-correct it
//     once it's out.
//
// The basic gate requires `confident=true` from
// translate.DetectThreadLanguage (at least 4 non-English messages making
// up >=40% of the recent ~30-message window). On top of that we layer a
// "most-recent-message wins" tiebreaker that's *intentionally weakened*
// in clearly-non-English threads:
//
//   - If the contact's overall history (looking at every incoming
//     non-trivial message, not just the recent window) is at least
//     allTimeNonEnglishFraction non-English, we treat the contact as
//     a non-English-default speaker and ignore the latest-message
//     tiebreaker. One English message from a contact who otherwise
//     speaks Spanish in 9 out of 10 messages doesn't change what
//     language the user wants to send back in.
//
//   - Otherwise (mixed-language contact, both languages routinely
//     used), defer to the most recent message. If the counterpart's
//     last reply was English we have an active English exchange in
//     progress and a Spanish auto-send would be jarring.
//
// We use the contact's *all-time* fraction (not just recent) for the
// strong-signal branch because slow-burn threads (a service contact who
// messages once a month) often have only 2-3 incoming messages total -
// any single English outlier swings a recent-window fraction by 30+
// points and trips the rule for the wrong reasons. All-time history is
// stable: a Spanish-speaking contact's earliest messages were Spanish,
// their newest are Spanish, and one English line in between doesn't
// change what they speak. Chatty threads are still dominated by recent
// activity in the all-time count too (because chatty == high message
// volume), so this doesn't make the system any less responsive to
// users who genuinely change languages mid-relationship.
//
// The user can always undo via revoke+resend, and the (EN) annotation
// shows what we sent, so the cost of a translate-when-undesired here is
// recoverable. The cost of *not* translating when the contact is
// clearly Spanish-default is much more annoying because the user has
// to manually translate every reply for the rest of the conversation.
func (sm *SessionManager) shouldAutoTranslateOutgoing(chatID string) bool {
	if sm.Translator == nil || !sm.Translator.IsReady() {
		return false
	}
	msgs := sm.db.GetMessages(chatID)
	return decideAutoTranslateOutgoing(msgs)
}

// decideAutoTranslateOutgoing is the pure decision logic for
// shouldAutoTranslateOutgoing, factored out so it can be unit-tested
// without standing up a Translator + DB. Operates on the chat's
// in-memory message list and uses only the public translate.* helpers.
//
// Decision flow, in order:
//
//  1. Compute the contact's all-time non-English fraction across every
//     incoming non-trivial message. If it's at or above
//     allTimeNonEnglishFraction (and we have at least
//     minIncomingForAllTime messages to compute it from), the contact
//     is a non-English-default speaker. Auto-translate, period.
//     This branch handles slow-burn threads (e.g. a service contact
//     who messages once a month in Spanish, then sends one English
//     line) where the recent-window confidence flag would say "not
//     enough samples" but the historical signal is unambiguous.
//
//  2. Fall back to the recent-window confidence flag from
//     translate.DetectThreadLanguage. If the recent thread isn't
//     confidently non-English we don't have enough signal to translate.
//
//  3. Recent-confident but not all-time-overwhelmingly non-English:
//     the contact uses both languages routinely. Defer to the most
//     recent incoming message - if it was English we're in an active
//     English exchange and shouldn't break it.
//
// (1) before (2) is intentional: all-time fraction is a strictly
// stronger signal than recent-window confidence (more data, less
// susceptible to a single outlier swinging the fraction), so when
// (1) fires, gating it on (2) would just make the system fail
// to translate for the wrong reason.
func decideAutoTranslateOutgoing(msgs []Message) bool {
	const allTimeNonEnglishFraction = 0.60
	const minIncomingForAllTime = 2

	// (1) Strong all-time signal: contact's history is overwhelmingly
	// non-English. Sufficient on its own.
	frac, examined := allTimeNonEnglishFractionOf(msgs)
	if examined >= minIncomingForAllTime && frac >= allTimeNonEnglishFraction {
		return true
	}

	// (2) Recent-window confidence required for the rest of the flow.
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if !m.FromMe && m.Text != "" {
			texts = append(texts, m.Text)
		}
	}
	threadLang, confident := translate.DetectThreadLanguage(texts)
	if !confident || threadLang == "" || threadLang == "en" {
		return false
	}

	// (3) Mixed-but-confident contact: defer to the most recent message.
	if last := lastIncomingNonTrivialOf(msgs); last != "" && translate.IsEnglish(last) {
		return false
	}
	return true
}

// allTimeNonEnglishFractionOf returns (fraction, examined): what share
// of every incoming non-trivial message in `msgs` classified as
// non-English, and how many messages went into the count.
//
// Looks at the *entire* stored message history rather than the recent
// window to stay stable on slow-burn threads where a single English
// outlier message would otherwise swing a recent-window fraction by 30+
// percentage points. Chatty threads remain dominated by their recent
// activity in this count too (because chatty == high message volume),
// so this doesn't make the system any less responsive to genuine
// mid-relationship language changes.
func allTimeNonEnglishFractionOf(msgs []Message) (float64, int) {
	examined := 0
	nonEnglish := 0
	for _, m := range msgs {
		if m.FromMe || m.Text == "" {
			continue
		}
		if translate.IsTrivialNoOp(m.Text) {
			continue
		}
		examined++
		code := translate.IdentifyMessageLanguage(m.Text)
		if code != "" && code != "en" {
			nonEnglish++
		}
	}
	if examined == 0 {
		return 0, 0
	}
	return float64(nonEnglish) / float64(examined), examined
}

// lastIncomingNonTrivialOf returns the text of the most recent incoming
// (i.e. !FromMe) non-trivial message in msgs, or "" if there is none.
// Used by decideAutoTranslateOutgoing for the latest-message tiebreaker.
func lastIncomingNonTrivialOf(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.FromMe || m.Text == "" {
			continue
		}
		if translate.IsTrivialNoOp(m.Text) {
			continue
		}
		return m.Text
	}
	return ""
}

func (sm *SessionManager) sendMedia(chatID, path string, kind MessageKind) error {
	if sm.client == nil || !sm.client.IsConnected() {
		return errors.New("not connected to WhatsApp")
	}

	data, mimeType, fileName, err := readUploadFile(path)
	if err != nil {
		return err
	}

	receiver, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid JID: %v", err)
	}

	uploadResp, err := sm.client.Upload(context.Background(), data, uploadMediaType(kind))
	if err != nil {
		return fmt.Errorf("failed to upload file: %v", err)
	}

	fileLength := uploadResp.FileLength
	raw := &waProto.Message{}
	switch kind {
	case MessageKindImage:
		raw.ImageMessage = &waProto.ImageMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	case MessageKindVideo:
		raw.VideoMessage = &waProto.VideoMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	case MessageKindAudio:
		raw.AudioMessage = &waProto.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
			PTT:           proto.Bool(false),
		}
	case MessageKindDocument:
		raw.DocumentMessage = &waProto.DocumentMessage{
			Mimetype:      proto.String(mimeType),
			Title:         proto.String(fileName),
			FileName:      proto.String(fileName),
			URL:           &uploadResp.URL,
			DirectPath:    &uploadResp.DirectPath,
			MediaKey:      uploadResp.MediaKey,
			FileEncSHA256: uploadResp.FileEncSHA256,
			FileSHA256:    uploadResp.FileSHA256,
			FileLength:    &fileLength,
		}
	default:
		return errors.New("unsupported media type")
	}

	sm.lastSent = time.Now()
	resp, err := sm.client.SendMessage(context.Background(), receiver, raw)
	if err != nil {
		return fmt.Errorf("failed to send media message: %v", err)
	}

	text := mediaDisplayText(kind, fileName, "")
	newMsg := sm.outgoingMessageFromSendResponse(resp, chatID, raw, kind, text, mimeType, fileName)
	sm.db.AddMessage(newMsg, false)
	if sm.currentReceiver == chatID {
		sm.uiHandler.NewMessage(newMsg)
	}
	sm.uiHandler.SetChats(sm.db.GetChatIds())

	// Same persistence rationale as sendText: outbound media must hit
	// disk now or it disappears across a backend restart, since WhatsApp
	// won't re-deliver our own already-ack'd send to us.
	sm.db.SaveChatCache()
	sm.db.SaveMessageCache()
	return nil
}

func (sm *SessionManager) outgoingMessageFromSendResponse(resp whatsmeow.SendResponse, chatID string, raw *waProto.Message, kind MessageKind, text, mimeType, fileName string) Message {
	selfID := ""
	if sm.client != nil && sm.client.Store != nil && sm.client.Store.ID != nil {
		selfID = sm.client.Store.ID.String()
	}

	contactID := chatID
	if strings.Contains(chatID, GROUPSUFFIX) {
		contactID = selfID
	}

	return Message{
		Id:           string(resp.ID),
		ChatId:       chatID,
		SenderId:     selfID,
		ContactId:    contactID,
		ContactName:  sm.db.GetIdName(contactID),
		ContactShort: sm.db.GetIdShort(contactID),
		Timestamp:    uint64(resp.Timestamp.Unix()),
		FromMe:       true,
		Text:         text,
		Kind:         kind,
		MimeType:     mimeType,
		FileName:     fileName,
		// SendMessage returned, so the server has ack'd it: one grey check.
		// Delivered/read upgrades arrive later as receipts.
		Status:     MessageStatusSent,
		RawMessage: raw,
	}
}

func notify(title, message string) error {
	if !config.Config.General.EnableNotifications {
		return nil
	} else if config.Config.General.UseTerminalBell {
		_, err := fmt.Printf("\a")
		return err
	}
	return beeep.Notify(title, message, "")
}

type eventHandler struct {
	sm *SessionManager
}

func (eh *eventHandler) Handle(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		eh.handleLiveMessage(v)
	case *events.HistorySync:
		eh.handleHistorySync(v)
	case *events.Connected:
		eh.sm.StatusChannel <- StatusMsg{true, nil}
	case *events.Disconnected:
		eh.sm.StatusChannel <- StatusMsg{false, nil}
	case *events.LoggedOut:
		eh.sm.StatusChannel <- StatusMsg{false, nil}
		eh.sm.uiHandler.PrintText("Logged out: " + fmt.Sprintf("%v", v.Reason))
	case *events.Archive:
		eh.sm.db.SetChatArchived(v.JID.String(), v.Action.GetArchived())
		eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		eh.sm.db.SaveChatCache()
	case *events.Pin:
		eh.sm.db.SetChatPinned(v.JID.String(), v.Action.GetPinned())
		eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		eh.sm.db.SaveChatCache()
	case *events.MarkChatAsRead:
		chatJID := eh.resolveLID(v.JID)
		chatID := chatJID.String()
		if os.Getenv("WHATSCLI_UNREAD_PROBE") != "" {
			fmt.Fprintf(os.Stdout, "[markread] chat=%s read=%v cutoff=%d fromFullSync=%v\n",
				chatID, v.Action.GetRead(), markReadCutoff(v), v.FromFullSync)
		}
		if v.Action.GetRead() {
			// Clear only up to the action's timestamp: during a full app-state
			// replay we see week-old read actions for chats that have since
			// received new (still unread) messages.
			eh.sm.db.MarkChatReadUpTo(chatID, markReadCutoff(v))
		} else {
			eh.sm.db.SetChatUnreadCount(chatID, 1)
		}
		eh.sm.scheduleChatRefresh()
	case *events.Receipt:
		eh.handleReceipt(v)
	case *events.OfflineSyncCompleted:
		eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		eh.sm.db.SaveChatCache()
		eh.sm.db.SaveMessageCache()
	}
}

func (eh *eventHandler) handleReceipt(evt *events.Receipt) {
	chatJID := eh.resolveLID(evt.Chat)
	chatID := chatJID.String()

	if os.Getenv("WHATSCLI_UNREAD_PROBE") != "" {
		fmt.Fprintf(os.Stdout, "[receipt] type=%q chat=%s sender=%s ids=%v ts=%s\n",
			evt.Type, chatID, evt.Sender, evt.MessageIDs, evt.Timestamp.Format("15:04:05"))
	}

	// Our own devices now send receipts from our LID identity, and whatsmeow
	// only maps PN-sender receipts to "read-self" — so a chat read on the
	// phone arrives here as a plain "read" from our own LID. Without this
	// check that read is mistaken for a PEER reading our messages, and the
	// unread badge never clears (observed live: sender=<own-lid>:25@lid).
	isSelf := false
	if eh.sm.client != nil && eh.sm.client.Store != nil {
		if own := eh.sm.client.Store.ID; own != nil && evt.Sender.User == own.User {
			isSelf = true
		}
		if ownLID := eh.sm.client.Store.LID; !ownLID.IsEmpty() && evt.Sender.User == ownLID.User {
			isSelf = true
		}
	}
	effectiveType := evt.Type
	if isSelf && evt.Type == types.ReceiptTypeRead {
		effectiveType = types.ReceiptTypeReadSelf
	}

	switch effectiveType {
	case types.ReceiptTypeDelivered:
		if isSelf {
			// Our own device ack — says nothing about the recipient.
			return
		}
		// The recipient's device has the message: two grey checks.
		eh.applyStatusUpgrade(chatID, evt.MessageIDs, MessageStatusDelivered)
		return
	case types.ReceiptTypeRead:
		// The recipient read it: two blue checks. In groups any member's
		// read receipt flips the ticks — simpler than tracking the full
		// member matrix, and right for the 1:1 chats that matter most.
		eh.applyStatusUpgrade(chatID, evt.MessageIDs, MessageStatusRead)
		return
	case types.ReceiptTypeReadSelf:
		break
	default:
		return
	}

	if effectiveType == types.ReceiptTypeReadSelf {
		// Read on the phone (or another linked device). Clear everything up
		// to the receipt time; anything newer genuinely hasn't been seen.
		cutoff := int64(0)
		if !evt.Timestamp.IsZero() {
			cutoff = evt.Timestamp.Unix()
		}
		eh.sm.db.MarkChatReadUpTo(chatID, cutoff)
		eh.sm.scheduleChatRefresh()
	}
}

// applyStatusUpgrade raises tick-mark state for our own messages and tells
// the client, skipping no-ops (receipts often repeat or arrive out of order).
func (eh *eventHandler) applyStatusUpgrade(chatID string, messageIDs []string, status MessageStatus) {
	changed := eh.sm.db.UpgradeMessageStatus(messageIDs, status)
	if len(changed) == 0 {
		return
	}
	eh.sm.uiHandler.MessageStatus(chatID, changed, status)
	eh.sm.scheduleChatRefresh()
}

// markReadCutoff extracts the "read up to" moment from a MarkChatAsRead
// action as Unix seconds. The action's message range is most precise; some
// clients send it in milliseconds, so normalize. Falls back to the event
// timestamp, then to 0 (= clear everything).
func markReadCutoff(v *events.MarkChatAsRead) int64 {
	ts := v.Action.GetMessageRange().GetLastMessageTimestamp()
	if ts > 1_000_000_000_000 {
		ts /= 1000
	}
	if ts == 0 && !v.Timestamp.IsZero() {
		ts = v.Timestamp.Unix()
	}
	return ts
}

// scheduleChatRefresh pushes the chat list to the UI and persists both caches
// after a short delay, coalescing event bursts. A full app-state resync emits
// one MarkChatAsRead per chat — hundreds of events — and SaveMessageCache
// rewrites the whole cache file, so doing this per-event would hammer the
// disk and flood the client with chat-list updates.
func (sm *SessionManager) scheduleChatRefresh() {
	sm.scheduleCacheRefresh(true)
}

// scheduleCacheRefresh persists history-sync changes while allowing callers
// to suppress an unchanged chat-list broadcast. The pending push bit is
// sticky across the coalescing window, so a later real chat change is never
// hidden by an earlier cache-only request.
func (sm *SessionManager) scheduleCacheRefresh(pushChats bool) {
	sm.chatRefreshMu.Lock()
	defer sm.chatRefreshMu.Unlock()
	sm.chatRefreshPushPending = sm.chatRefreshPushPending || pushChats
	if sm.chatRefreshTimer != nil {
		// A flush is already pending and will pick this change up too. Not
		// resetting the timer makes this a throttle, not a debounce: during
		// a continuous event stream (initial pairing) the UI still gets a
		// refresh every 400ms instead of starving until the stream ends.
		return
	}
	sm.chatRefreshTimer = time.AfterFunc(400*time.Millisecond, func() {
		sm.chatRefreshMu.Lock()
		shouldPushChats := sm.chatRefreshPushPending
		sm.chatRefreshPushPending = false
		sm.chatRefreshTimer = nil
		sm.chatRefreshMu.Unlock()
		started := time.Now()
		fmt.Fprintf(os.Stdout, "[cache-refresh] started push_chats=%t\n", shouldPushChats)
		if shouldPushChats {
			sm.uiHandler.SetChats(sm.db.GetChatIds())
		}
		sm.db.SaveChatCache()
		sm.db.SaveMessageCache()
		fmt.Fprintf(os.Stdout, "[cache-refresh] completed push_chats=%t duration_ms=%d\n", shouldPushChats, time.Since(started).Milliseconds())
	})
}

func (eh *eventHandler) handleLiveMessage(evt *events.Message) {
	if eh.handleLiveReaction(evt) {
		return
	}

	msg, action, ok := eh.normalizeEventMessage(evt)
	if !ok {
		return
	}

	switch action {
	case "revoke":
		if eh.sm.db.MarkMessageRevoked(msg.Id) && eh.sm.currentReceiver == msg.ChatId {
			eh.sm.uiHandler.NewScreen(eh.sm.getMessages(msg.ChatId))
		}
		eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
		return
	case "ignore":
		return
	}

	// A message landing in the open chat is normally read on the spot — but
	// if the user only just arrowed onto the chat (dwell timer still armed),
	// flag it unread so the timer's batch decides, same as the rest.
	inDwell := eh.sm.hasPendingMarkRead(msg.ChatId)
	markUnread := !msg.FromMe && (msg.ChatId != eh.sm.currentReceiver || inDwell)
	isNew := eh.sm.db.AddMessage(msg, markUnread)
	// Replying from ANY device means the user has read the chat — WhatsApp
	// clears the badge on reply even when the read-self receipt never
	// reaches us. Clear up to the reply's own timestamp only, so anything
	// arriving after the reply still counts as unread.
	if msg.FromMe {
		eh.sm.db.MarkChatReadUpTo(msg.ChatId, int64(msg.Timestamp))
	}
	// Read receipt for an arrival in the settled current chat — this is
	// what clears the PHONE's badge (and gives the sender blue ticks); the
	// local database never flags it, so no other path will send one.
	if isNew && !markUnread && !msg.FromMe {
		go eh.sm.sendReadReceipt(msg)
	}
	if msg.ChatId == eh.sm.currentReceiver {
		if isNew {
			eh.sm.uiHandler.NewMessage(msg)
		} else {
			eh.sm.uiHandler.NewScreen(eh.sm.getMessages(msg.ChatId))
		}
	} else if markUnread && msg.Timestamp > uint64(time.Now().Unix()-30) {
		if err := notify(msg.ContactShort, msg.Text); err != nil {
			eh.sm.uiHandler.PrintError(err)
		}
	}
	eh.sm.uiHandler.SetChats(eh.sm.db.GetChatIds())
	eh.sm.db.SaveChatCache()
	eh.sm.db.SaveMessageCache()

	if isNew && msg.Kind == MessageKindAudio && eh.sm.Transcriber != nil && eh.sm.Transcriber.IsReady() {
		go eh.sm.transcribeAndTranslate(msg)
	} else if isNew && msg.Kind == MessageKindText && msg.Text != "" && eh.sm.Translator != nil && eh.sm.Translator.IsReady() {
		go func() {
			eh.sm.translateIncoming(msg)
			eh.sm.db.SaveTranslationCache()
		}()
	}
}

// handleLiveReaction consumes reaction protocol messages before ordinary
// message normalization (which intentionally ignores protocol-only payloads).
// It returns true whenever evt is a reaction, including malformed/decryption
// failures, so a reaction can never surface as an "unsupported" chat message.
func (eh *eventHandler) handleLiveReaction(evt *events.Message) bool {
	if evt == nil || evt.Message == nil {
		return false
	}
	reaction := evt.Message.GetReactionMessage()
	if reaction == nil && evt.Message.GetEncReactionMessage() != nil {
		if eh.sm.client == nil {
			fmt.Fprintln(os.Stdout, "[reaction] ignored encrypted reaction: client unavailable")
			return true
		}
		decrypted, err := eh.sm.client.DecryptReaction(context.Background(), evt)
		if err != nil {
			fmt.Fprintf(os.Stdout, "[reaction] decrypt failed error=%q\n", err)
			return true
		}
		reaction = decrypted
	}
	if reaction == nil {
		return false
	}

	targetID := reaction.GetKey().GetID()
	senderID := eh.resolveLID(evt.Info.Sender).String()
	if evt.Info.IsFromMe {
		senderID = "me"
	}
	if targetID == "" || senderID == "" {
		fmt.Fprintf(os.Stdout, "[reaction] ignored malformed target_present=%t sender_present=%t\n", targetID != "", senderID != "")
		return true
	}

	updated, changed := eh.sm.db.ApplyMessageReaction(targetID, senderID, reaction.GetText())
	if !changed {
		if _, exists := eh.sm.db.GetMessage(targetID); !exists {
			fmt.Fprintln(os.Stdout, "[reaction] target missing; awaiting history sync")
		}
		return true
	}
	if updated.ChatId == eh.sm.currentReceiver {
		eh.sm.uiHandler.NewScreen(eh.sm.getMessages(updated.ChatId))
	}
	eh.sm.scheduleCacheRefresh(false)
	action := "set"
	if reaction.GetText() == "" {
		action = "remove"
	}
	fmt.Fprintf(os.Stdout, "[reaction] applied action=%s open_chat=%t\n", action, updated.ChatId == eh.sm.currentReceiver)
	return true
}

func (eh *eventHandler) handleHistorySync(evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		return
	}
	started := time.Now()
	currentReceiver := eh.sm.currentReceiver
	currentBefore := eh.sm.getMessages(currentReceiver)
	chatsBefore := eh.sm.db.GetChatIds()
	currentConversationIncluded := false
	addedMessages := 0
	reactionChanges := 0
	reactionCount := 0
	fmt.Fprintf(
		os.Stdout,
		"[history-sync] started type=%s conversations=%d current_open=%t\n",
		evt.Data.GetSyncType().String(),
		len(evt.Data.GetConversations()),
		currentReceiver != "",
	)

	if os.Getenv("WHATSCLI_UNREAD_PROBE") != "" {
		fmt.Fprintf(os.Stdout, "[unread-probe] history sync type=%v conversations=%d\n",
			evt.Data.GetSyncType(), len(evt.Data.GetConversations()))
		for _, conv := range evt.Data.GetConversations() {
			fmt.Fprintf(os.Stdout, "[unread-probe]   conv=%s unread=%d markedUnread=%v msgs=%d\n",
				conv.GetID(), conv.GetUnreadCount(), conv.GetMarkedAsUnread(), len(conv.GetMessages()))
		}
	}

	for _, conv := range evt.Data.GetConversations() {
		chatID := conv.GetID()
		if chatID == "" {
			chatID = conv.GetNewJID()
		}
		if chatID == "" {
			continue
		}

		chatJID, err := types.ParseJID(chatID)
		if err != nil {
			continue
		}
		// On-demand responses key migrated chats by LID; fold them back onto
		// the phone-number chat we track, or the update lands on a hidden
		// duplicate entry.
		chatJID = eh.resolveLID(chatJID)
		chatID = chatJID.String()
		if chatID == currentReceiver {
			currentConversationIncluded = true
		}

		chatName := conv.GetName()
		if chatName == "" {
			chatName = conv.GetDisplayName()
		}
		if chatName == "" {
			chatName = eh.sm.getChatName(chatJID)
		}

		lastMessage := int64(conv.GetLastMsgTimestamp())
		if lastMessage == 0 {
			lastMessage = int64(conv.GetConversationTimestamp())
		}
		eh.sm.db.AddChat(Chat{
			Id:          chatID,
			IsGroup:     chatJID.Server == types.GroupServer,
			Name:        chatName,
			Unread:      int(conv.GetUnreadCount()),
			LastMessage: lastMessage,
			Archived:    conv.GetArchived(),
			Pinned:      conv.GetPinned() > 0,
		})

		for _, histMsg := range conv.GetMessages() {
			webMsg := histMsg.GetMessage()
			if webMsg == nil {
				continue
			}
			parsed, err := eh.sm.client.ParseWebMessage(chatJID, webMsg)
			if err != nil {
				continue
			}
			msg, action, ok := eh.normalizeEventMessage(parsed)
			if !ok || action != "" {
				continue
			}
			if msg.FromMe {
				msg.Status = statusFromWebInfo(webMsg.GetStatus())
			}
			if eh.sm.db.AddMessage(msg, false) {
				addedMessages++
			}
			reactions := eh.historyReactions(webMsg.GetReactions(), chatJID)
			reactionCount += len(reactions)
			if eh.sm.db.SetMessageReactions(msg.Id, reactions) {
				reactionChanges++
			}
		}
		if evt.Data.GetSyncType() == waHistorySync.HistorySync_ON_DEMAND {
			// This conversation came from an explicit per-chat request to the
			// primary phone, so its state is authoritative — including zero.
			// Ignoring zero made the local counter a one-way ratchet: a chat
			// read on the phone while this client was offline stayed unread
			// forever unless a read receipt happened to be replayed later.
			n := int(conv.GetUnreadCount())
			if conv.GetMarkedAsUnread() && n == 0 {
				// WhatsApp represents a manual "mark unread" separately from
				// unread message count. The current client model uses one badge
				// for both states, so preserve that marker as a count of one.
				n = 1
			}
			eh.sm.db.UpdateChatUnread(chatID, n)
		} else {
			// Pairing-time history sync: authoritative snapshot, apply exactly.
			eh.sm.db.UpdateChatUnread(chatID, int(conv.GetUnreadCount()))
			if conv.GetMarkedAsUnread() && conv.GetUnreadCount() == 0 {
				eh.sm.db.SetChatUnreadCount(chatID, 1)
			}
		}
	}

	currentAfter := eh.sm.getMessages(currentReceiver)
	chatsAfter := eh.sm.db.GetChatIds()
	currentChanged := currentConversationIncluded && !messageScreensEqual(currentBefore, currentAfter)
	chatsChanged := !chatScreensEqual(chatsBefore, chatsAfter)

	// Coalesced: a startup reconciliation sweep delivers dozens of history
	// syncs in a burst, and SaveMessageCache rewrites the whole cache file.
	// Persist every response, but only broadcast the 1,000+ row chat list when
	// its visible state changed. Likewise, never republish the open thread for
	// a response belonging to one of the other chats in the sweep.
	eh.sm.scheduleCacheRefresh(chatsChanged)
	currentScreenPushed := currentChanged && eh.sm.currentReceiver == currentReceiver
	if currentScreenPushed {
		eh.sm.uiHandler.NewScreen(currentAfter)
	}
	fmt.Fprintf(
		os.Stdout,
		"[history-sync] completed type=%s conversations=%d added_messages=%d reactions=%d reaction_changes=%d chats_changed=%t current_included=%t current_changed=%t screen_pushed=%t duration_ms=%d\n",
		evt.Data.GetSyncType().String(),
		len(evt.Data.GetConversations()),
		addedMessages,
		reactionCount,
		reactionChanges,
		chatsChanged,
		currentConversationIncluded,
		currentChanged,
		currentScreenPushed,
		time.Since(started).Milliseconds(),
	)
}

// historyReactions converts the authoritative reaction metadata attached to a
// history message. The reaction key identifies the reacting participant (and
// whether it was us); the containing WebMessageInfo identifies the target.
func (eh *eventHandler) historyReactions(items []*waWeb.Reaction, chatJID types.JID) []MessageReaction {
	reactions := make([]MessageReaction, 0, len(items))
	for _, item := range items {
		if item == nil || item.GetKey() == nil || strings.TrimSpace(item.GetText()) == "" {
			continue
		}
		key := item.GetKey()
		senderID := ""
		switch {
		case key.GetFromMe():
			senderID = "me"
		case key.GetParticipant() != "":
			if participant, err := types.ParseJID(key.GetParticipant()); err == nil {
				senderID = eh.resolveLID(participant).String()
			} else {
				senderID = canonicalMessageJID(key.GetParticipant())
			}
		default:
			// In a one-to-one chat WhatsApp omits Participant; the other
			// party is the chat itself.
			senderID = eh.resolveLID(chatJID).String()
		}
		reactions = append(reactions, MessageReaction{SenderId: senderID, Emoji: item.GetText()})
	}
	return normalizedReactions(reactions)
}

// messageScreensEqual compares exactly the fields sent to the GUI. RawMessage
// is intentionally excluded: it is backend-only media/protocol data and a
// pointer change must not trigger a full SwiftUI conversation rebuild.
func messageScreensEqual(a, b []Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Id != y.Id || x.ChatId != y.ChatId || x.SenderId != y.SenderId ||
			x.ContactId != y.ContactId || x.ContactName != y.ContactName ||
			x.ContactShort != y.ContactShort || x.Timestamp != y.Timestamp ||
			x.FromMe != y.FromMe || x.Forwarded != y.Forwarded || x.Text != y.Text ||
			x.Kind != y.Kind || x.MimeType != y.MimeType || x.FileName != y.FileName ||
			x.Unread != y.Unread || x.Status != y.Status ||
			!messageReactionsEqual(x.Reactions, y.Reactions) {
			return false
		}
	}
	return true
}

func chatScreensEqual(a, b []Chat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// statusFromWebInfo maps a history-sync message's WebMessageInfo status to
// our tick-mark states.
func statusFromWebInfo(s waWeb.WebMessageInfo_Status) MessageStatus {
	switch s {
	case waWeb.WebMessageInfo_SERVER_ACK:
		return MessageStatusSent
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return MessageStatusDelivered
	case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
		return MessageStatusRead
	default:
		return MessageStatusSent
	}
}

func (eh *eventHandler) normalizeEventMessage(evt *events.Message) (Message, string, bool) {
	if evt == nil || evt.Message == nil {
		return Message{}, "ignore", false
	}

	if protocol := evt.Message.GetProtocolMessage(); protocol != nil {
		if protocol.GetType() == waProto.ProtocolMessage_REVOKE && protocol.GetKey() != nil {
			return Message{
				Id:     protocol.GetKey().GetID(),
				ChatId: eh.resolveLID(evt.Info.Chat).String(),
			}, "revoke", true
		}
		return Message{}, "ignore", false
	}

	msg, ok := eh.messageFromInfo(evt.Info, evt.Message)
	return msg, "", ok
}

func (eh *eventHandler) messageFromInfo(info types.MessageInfo, raw *waProto.Message) (Message, bool) {
	if raw == nil {
		return Message{}, false
	}

	resolvedChat := eh.resolveLID(info.Chat)
	chatID := resolvedChat.String()
	if chatID == "" {
		return Message{}, false
	}

	contactID, contactName, contactShort := eh.contactForMessage(info)
	msg := Message{
		Id:           string(info.ID),
		ChatId:       chatID,
		SenderId:     eh.resolveLID(info.Sender).String(),
		ContactId:    contactID,
		ContactName:  contactName,
		ContactShort: contactShort,
		Timestamp:    uint64(info.Timestamp.Unix()),
		FromMe:       info.IsFromMe,
		RawMessage:   raw,
	}
	if info.IsFromMe {
		// Live echo of our own message (from this or another device) —
		// the server clearly has it. Receipts upgrade from here.
		msg.Status = MessageStatusSent
	}

	switch {
	case raw.GetConversation() != "":
		msg.Kind = MessageKindText
		msg.Text = raw.GetConversation()
		return msg, true
	case raw.GetExtendedTextMessage() != nil:
		ext := raw.GetExtendedTextMessage()
		msg.Kind = MessageKindText
		msg.Text = ext.GetText()
		msg.Forwarded = ext.GetContextInfo().GetIsForwarded()
		return msg, true
	case raw.GetImageMessage() != nil:
		image := raw.GetImageMessage()
		msg.Kind = MessageKindImage
		msg.MimeType = image.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindImage, "", image.GetCaption())
		msg.Forwarded = image.GetContextInfo().GetIsForwarded()
		return msg, true
	case raw.GetVideoMessage() != nil:
		video := raw.GetVideoMessage()
		msg.Kind = MessageKindVideo
		msg.MimeType = video.GetMimetype()
		msg.Text = mediaDisplayText(MessageKindVideo, "", video.GetCaption())
		msg.Forwarded = video.GetContextInfo().GetIsForwarded()
		return msg, true
	case raw.GetAudioMessage() != nil:
		audio := raw.GetAudioMessage()
		msg.Kind = MessageKindAudio
		msg.MimeType = audio.GetMimetype()
		dur := audio.GetSeconds()
		if dur > 0 {
			msg.Text = fmt.Sprintf("[AUDIO %d:%02d]", dur/60, dur%60)
		} else {
			msg.Text = mediaDisplayText(MessageKindAudio, "", "")
		}
		msg.Forwarded = audio.GetContextInfo().GetIsForwarded()
		return msg, true
	case raw.GetDocumentMessage() != nil:
		doc := raw.GetDocumentMessage()
		msg.Kind = MessageKindDocument
		msg.MimeType = doc.GetMimetype()
		msg.FileName = doc.GetFileName()
		msg.Text = mediaDisplayText(MessageKindDocument, doc.GetFileName(), doc.GetCaption())
		msg.Forwarded = doc.GetContextInfo().GetIsForwarded()
		return msg, true
	default:
		return Message{}, false
	}
}

func (eh *eventHandler) contactForMessage(info types.MessageInfo) (string, string, string) {
	var source types.JID
	if info.IsGroup {
		source = info.Sender
	} else {
		source = info.Chat
	}

	resolved := eh.resolveLID(source)
	contactID := resolved.String()
	name := eh.getContactName(source)
	short := eh.getContactShort(source)

	// The message's notify/push name is how native WhatsApp can label an
	// unsaved group participant (shown there with a leading "~"). Prefer a
	// saved contact name when one exists, but replace bare phone/JID fallbacks
	// with the sender-provided name. ParseWebMessage fills this for history as
	// well as live events.
	if pushName := usablePushName(info.PushName); pushName != "" {
		if isFallbackContactLabel(name, contactID) {
			name = pushName
		}
		if isFallbackContactLabel(short, contactID) {
			short = pushName
		}
	}

	return contactID, name, short
}

func (eh *eventHandler) resolveLID(jid types.JID) types.JID {
	// Message participant JIDs may include a linked-device suffix (e.g.
	// 15551234567:25@s.whatsapp.net), while contact and LID stores are keyed
	// by the canonical user JID. Keeping that suffix caused both failed name
	// lookups and the visible ":25" label.
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.LIDs != nil {
		pn, err := eh.sm.client.Store.LIDs.GetPNForLID(context.Background(), jid)
		if err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return jid
}

func usablePushName(name string) string {
	name = strings.TrimSpace(name)
	if name == "-" || name == "username" {
		return ""
	}
	return name
}

func (eh *eventHandler) getContactName(jid types.JID) string {
	jid = eh.resolveLID(jid)
	if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.Contacts != nil {
		contact, err := eh.sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				return contact.FullName
			}
			if contact.PushName != "" {
				return contact.PushName
			}
		}
	}
	return eh.sm.db.GetIdName(jid.String())
}

func (eh *eventHandler) getContactShort(jid types.JID) string {
	jid = eh.resolveLID(jid)
	if eh.sm.client != nil && eh.sm.client.Store != nil && eh.sm.client.Store.Contacts != nil {
		contact, err := eh.sm.client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.Found {
			if contact.PushName != "" {
				return contact.PushName
			}
			if contact.FullName != "" {
				return contact.FullName
			}
		}
	}
	return eh.sm.db.GetIdShort(jid.String())
}

func (sm *SessionManager) downloadMessage(msg Message, preview bool) (string, error) {
	if sm.client == nil || !sm.client.IsConnected() {
		return "", errors.New("not connected to WhatsApp")
	}

	downloadable, err := downloadableFromMessage(msg)
	if err != nil {
		return "", err
	}

	baseDir := config.Config.General.DownloadPath
	if preview {
		baseDir = config.Config.General.PreviewPath
	}
	if err = os.MkdirAll(baseDir, 0o755); err != nil {
		return "", err
	}

	fileName := downloadFileName(msg)
	fullPath := filepath.Join(baseDir, fileName)
	if _, err = os.Stat(fullPath); err == nil {
		return fullPath, nil
	}

	data, err := sm.client.Download(context.Background(), downloadable)
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", err
	}
	return fullPath, nil
}

func downloadableFromMessage(msg Message) (whatsmeow.DownloadableMessage, error) {
	if msg.RawMessage == nil {
		return nil, errors.New("This is not a downloadable message")
	}
	switch msg.Kind {
	case MessageKindImage:
		if media := msg.RawMessage.GetImageMessage(); media != nil {
			return media, nil
		}
	case MessageKindVideo:
		if media := msg.RawMessage.GetVideoMessage(); media != nil {
			return media, nil
		}
	case MessageKindAudio:
		if media := msg.RawMessage.GetAudioMessage(); media != nil {
			return media, nil
		}
	case MessageKindDocument:
		if media := msg.RawMessage.GetDocumentMessage(); media != nil {
			return media, nil
		}
	}
	return nil, errors.New("This is not a downloadable message")
}

func readUploadFile(path string) ([]byte, string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", err
	}
	fileName := filepath.Base(path)
	mimeType := detectMimeType(path, data)
	return data, mimeType, fileName, nil
}

func detectMimeType(path string, data []byte) string {
	if len(data) == 0 {
		if extType := mime.TypeByExtension(filepath.Ext(path)); extType != "" {
			return stripMimeParams(extType)
		}
		return "application/octet-stream"
	}
	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}
	detected := stripMimeParams(http.DetectContentType(sample))
	if extType := mime.TypeByExtension(filepath.Ext(path)); extType != "" {
		extType = stripMimeParams(extType)
		if detected == "application/octet-stream" || strings.HasPrefix(extType, "audio/") || strings.HasPrefix(extType, "video/") {
			return extType
		}
	}
	return detected
}

func stripMimeParams(value string) string {
	if idx := strings.Index(value, ";"); idx >= 0 {
		return value[:idx]
	}
	return value
}

var preferredExtensions = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"video/mp4":  ".mp4",
	"audio/ogg":  ".ogg",
	"audio/mpeg": ".mp3",
}

func downloadFileName(msg Message) string {
	if msg.FileName != "" {
		return msg.FileName
	}
	ext := ""
	if msg.MimeType != "" {
		base := msg.MimeType
		if i := strings.Index(base, ";"); i >= 0 {
			base = strings.TrimSpace(base[:i])
		}
		if preferred, ok := preferredExtensions[base]; ok {
			ext = preferred
		} else if exts, err := mime.ExtensionsByType(msg.MimeType); err == nil && len(exts) > 0 {
			ext = exts[len(exts)-1]
		}
	}
	return msg.Id + ext
}

func uploadMediaType(kind MessageKind) whatsmeow.MediaType {
	switch kind {
	case MessageKindImage:
		return whatsmeow.MediaImage
	case MessageKindVideo:
		return whatsmeow.MediaVideo
	case MessageKindAudio:
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}

func commandNameForKind(kind MessageKind) string {
	switch kind {
	case MessageKindImage:
		return "sendimage"
	case MessageKindVideo:
		return "sendvideo"
	case MessageKindAudio:
		return "sendaudio"
	default:
		return "upload"
	}
}

func mediaDisplayText(kind MessageKind, fileName, caption string) string {
	label := "[FILE]"
	switch kind {
	case MessageKindImage:
		label = "[IMAGE]"
	case MessageKindVideo:
		label = "[VIDEO]"
	case MessageKindAudio:
		label = "[AUDIO]"
	case MessageKindDocument:
		label = "[DOCUMENT]"
	}
	parts := []string{label}
	if fileName != "" && kind == MessageKindDocument {
		parts = append(parts, fileName)
	}
	if caption != "" {
		parts = append(parts, caption)
	}
	return strings.Join(parts, " ")
}
