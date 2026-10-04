package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/antong314/whatscli-rs/backend/config"
	"go.mau.fi/whatsmeow/types"
)

// On-demand history re-fetch.
//
// Normal operation never wakes the phone for history (see
// requestChatHistorySync). RefetchHistory is an explicit, operator-driven
// backfill: for each chat it pages backwards through the phone's copy of the
// conversation with HISTORY_SYNC_ON_DEMAND requests, re-anchoring on the
// oldest message of every response. Responses flow through the ordinary
// handleHistorySync path, so messages are normalized exactly like live and
// pairing-time history (including LID→PN chat resolution) and deduplicated by
// ID — only messages missing from the cache are added.

const (
	defaultRefetchPageSize  = 50
	defaultRefetchTimeout   = 60 * time.Second
	defaultRefetchDelay     = 3 * time.Second
	defaultRefetchMaxPages  = 200
	defaultRefetchSaveEvery = 5
	// refetchInitialAnchors is how many of the newest cached messages are
	// tried as the first anchor. The phone answers nothing when it does not
	// know the anchor (e.g. a message deleted for everyone), so falling back
	// to the next-newest message avoids giving up on the whole chat.
	refetchInitialAnchors = 3
)

// errHistoryTimeout marks a request whose ON_DEMAND response never arrived.
var errHistoryTimeout = errors.New("timed out waiting for on-demand history response")

// HistoryRefetchOptions configures RefetchHistory. Zero values pick defaults.
type HistoryRefetchOptions struct {
	ChatIDs []string
	// Since stops paging a chat once a response reaches messages older than
	// this instant. Zero means no lower bound (MaxPages still applies).
	Since time.Time
	// PageSize is the message count asked for per on-demand request.
	PageSize int
	// Timeout is how long to wait for each chat's ON_DEMAND response.
	Timeout time.Duration
	// Delay is the pause between consecutive requests, so the phone is not
	// hammered.
	Delay time.Duration
	// MaxPages caps the number of requests per chat.
	MaxPages int
	// SaveEvery persists the message cache every N pages (in addition to
	// after each chat).
	SaveEvery int
	// Logf receives progress lines. Defaults to stdout.
	Logf func(format string, args ...any)
}

func (o *HistoryRefetchOptions) applyDefaults() {
	if o.PageSize <= 0 {
		o.PageSize = defaultRefetchPageSize
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultRefetchTimeout
	}
	if o.Delay < 0 {
		o.Delay = 0
	}
	if o.MaxPages <= 0 {
		o.MaxPages = defaultRefetchMaxPages
	}
	if o.SaveEvery <= 0 {
		o.SaveEvery = defaultRefetchSaveEvery
	}
	if o.Logf == nil {
		o.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stdout, format+"\n", args...)
		}
	}
}

// HistoryRefetchResult summarizes one chat's re-fetch.
type HistoryRefetchResult struct {
	ChatID string
	// Pages is the number of responses received.
	Pages int
	// Returned is the number of messages the phone sent back in total
	// (including ones already cached and non-displayable ones).
	Returned int
	// Added is the number of messages newly inserted into the cache.
	Added int
	// OldestTimestamp is the oldest message timestamp reached (unix seconds).
	OldestTimestamp uint64
	StopReason      string
	Err             error
}

// historyAnchor identifies the message an on-demand request pages back from.
type historyAnchor struct {
	ID        string
	FromMe    bool
	Timestamp uint64
}

// historyPage is what one ON_DEMAND history-sync response contained for one
// conversation, published by handleHistorySync to RefetchHistory.
type historyPage struct {
	ChatID   string
	Returned int
	Added    int
	// Oldest is the oldest message in the response (zero when Returned is
	// 0). It is taken from every parsed message, including reactions and
	// unsupported payloads, because any of them is a valid paging anchor.
	Oldest historyAnchor
}

// observe folds one parsed history message into the page summary.
func (p *historyPage) observe(info types.MessageInfo) {
	if info.ID == "" {
		return
	}
	p.Returned++
	ts := info.Timestamp.Unix()
	if ts <= 0 {
		return
	}
	if p.Oldest.ID == "" || uint64(ts) < p.Oldest.Timestamp {
		p.Oldest = historyAnchor{ID: string(info.ID), FromMe: info.IsFromMe, Timestamp: uint64(ts)}
	}
}

type historyWaiter struct {
	keys map[string]bool
	ch   chan historyPage
}

// subscribeHistory registers interest in ON_DEMAND responses for any of the
// given chat IDs. Subscribe before sending the request so a fast response is
// never missed. The returned func unregisters.
func (sm *SessionManager) subscribeHistory(keys ...string) (<-chan historyPage, func()) {
	waiter := &historyWaiter{keys: make(map[string]bool, len(keys)), ch: make(chan historyPage, 8)}
	for _, key := range keys {
		if key != "" {
			waiter.keys[key] = true
		}
	}
	sm.historyMu.Lock()
	if sm.historyWaiters == nil {
		sm.historyWaiters = make(map[uint64]*historyWaiter)
	}
	id := sm.historyNextID
	sm.historyNextID++
	sm.historyWaiters[id] = waiter
	sm.historyMu.Unlock()
	return waiter.ch, func() {
		sm.historyMu.Lock()
		delete(sm.historyWaiters, id)
		sm.historyMu.Unlock()
	}
}

// publishHistoryPage hands an ON_DEMAND response to any waiter registered
// for one of the keys (the resolved chat ID and the raw conversation ID,
// which may be an unresolved LID). Never blocks. Notified waiter IDs are
// recorded in notified (when non-nil) for publishHistoryMisses.
func (sm *SessionManager) publishHistoryPage(page historyPage, notified map[uint64]bool, keys ...string) {
	sm.historyMu.Lock()
	defer sm.historyMu.Unlock()
	for id, waiter := range sm.historyWaiters {
		for _, key := range keys {
			if key != "" && waiter.keys[key] {
				select {
				case waiter.ch <- page:
				default:
				}
				if notified != nil {
					notified[id] = true
				}
				break
			}
		}
	}
}

// publishHistoryMisses tells every waiter not served by an ON_DEMAND
// response that the response held nothing for its chat. Normal operation
// sends no on-demand requests (see requestChatHistorySync), so while a
// re-fetch runs an ON_DEMAND response without the requested chat means the
// phone had nothing older to give — waiting out the timeout would only
// slow the end of every chat.
func (sm *SessionManager) publishHistoryMisses(notified map[uint64]bool) {
	sm.historyMu.Lock()
	defer sm.historyMu.Unlock()
	for id, waiter := range sm.historyWaiters {
		if notified[id] {
			continue
		}
		select {
		case waiter.ch <- historyPage{}:
		default:
		}
	}
}

// markHistoryReady signals that the startup cache load has finished, so a
// re-fetch can anchor on cached messages. Safe to call repeatedly.
func (sm *SessionManager) markHistoryReady() {
	sm.historyMu.Lock()
	defer sm.historyMu.Unlock()
	if sm.historyReady == nil {
		sm.historyReady = make(chan struct{})
	}
	select {
	case <-sm.historyReady:
	default:
		close(sm.historyReady)
	}
}

func (sm *SessionManager) historyReadyChan() <-chan struct{} {
	sm.historyMu.Lock()
	defer sm.historyMu.Unlock()
	if sm.historyReady == nil {
		sm.historyReady = make(chan struct{})
	}
	return sm.historyReady
}

// WaitUntilReady blocks until the session is connected and the startup chat
// and message caches are loaded, the session turns out to be unpaired, the
// context is cancelled, or timeout elapses.
func (sm *SessionManager) WaitUntilReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	ready := sm.historyReadyChan()
	for {
		select {
		case <-ready:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("WhatsApp session not ready after %s (is the network up and the phone online?)", timeout)
		case <-ticker.C:
			if c := sm.client; c != nil && c.Store != nil && c.Store.ID == nil {
				return errors.New("this WhatsApp session is not paired; start the app normally and scan the QR code first")
			}
		}
	}
}

// SaveCaches persists the chat and message caches to disk.
func (sm *SessionManager) SaveCaches() {
	sm.db.SaveChatCache()
	sm.db.SaveMessageCache()
}

// Shutdown flushes caches and disconnects from WhatsApp.
func (sm *SessionManager) Shutdown() {
	sm.chatRefreshMu.Lock()
	if sm.chatRefreshTimer != nil {
		sm.chatRefreshTimer.Stop()
		sm.chatRefreshTimer = nil
	}
	sm.chatRefreshMu.Unlock()
	sm.SaveCaches()
	if sm.client != nil {
		sm.client.Disconnect()
	}
}

func (sm *SessionManager) sendHistoryRequest(chatID string, anchor historyAnchor, count int) error {
	if sm.historyRequestFn != nil {
		return sm.historyRequestFn(chatID, anchor, count)
	}
	return sm.requestHistoryBeforeMessage(chatID, anchor.ID, anchor.FromMe, anchor.Timestamp, count)
}

// requestHistoryPage sends one on-demand request and waits for the matching
// chat's ON_DEMAND response.
func (sm *SessionManager) requestHistoryPage(ctx context.Context, chatID string, anchor historyAnchor, opts HistoryRefetchOptions) (historyPage, error) {
	pages, unsubscribe := sm.subscribeHistory(sm.historyKeysForChat(chatID)...)
	defer unsubscribe()
	if err := sm.sendHistoryRequest(chatID, anchor, opts.PageSize); err != nil {
		return historyPage{}, fmt.Errorf("send history request: %w", err)
	}
	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()
	select {
	case page := <-pages:
		return page, nil
	case <-timer.C:
		return historyPage{}, errHistoryTimeout
	case <-ctx.Done():
		return historyPage{}, ctx.Err()
	}
}

// historyKeysForChat lists the IDs an ON_DEMAND response for chatID may be
// keyed by: the chat itself plus, for a phone-number chat, its LID
// (requestHistoryBeforeMessage asks by LID when one is known).
func (sm *SessionManager) historyKeysForChat(chatID string) []string {
	keys := []string{chatID}
	jid, err := types.ParseJID(chatID)
	if err != nil || jid.Server != types.DefaultUserServer {
		return keys
	}
	if c := sm.client; c != nil && c.Store != nil && c.Store.LIDs != nil {
		if lid, lidErr := c.Store.LIDs.GetLIDForPN(context.Background(), jid); lidErr == nil && !lid.IsEmpty() {
			keys = append(keys, lid.String())
		}
	}
	return keys
}

// initialRefetchAnchors returns up to n of the newest cached messages of a
// chat, newest first, skipping entries without a usable ID or timestamp.
func (sm *SessionManager) initialRefetchAnchors(chatID string, n int) []historyAnchor {
	msgs := sm.db.LatestMessages(chatID, n*4)
	anchors := make([]historyAnchor, 0, n)
	for _, msg := range msgs {
		if msg.Id == "" || msg.Timestamp == 0 {
			continue
		}
		anchors = append(anchors, historyAnchor{ID: msg.Id, FromMe: msg.FromMe, Timestamp: msg.Timestamp})
		if len(anchors) == n {
			break
		}
	}
	return anchors
}

// messageSecretAnchor returns the newest message ID of chatID recorded in
// whatsmeow's message-secret table (same source as loadBacklog's fallback).
// The table has no timestamp, so the anchor uses "now".
func (sm *SessionManager) messageSecretAnchor(chatID string) (historyAnchor, bool) {
	dbPath := config.GetSessionFilePath() + ".db"
	if _, err := os.Stat(dbPath); err != nil {
		return historyAnchor{}, false
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_foreign_keys=on&mode=ro")
	if err != nil {
		return historyAnchor{}, false
	}
	defer db.Close()
	var senderJID, messageID string
	err = db.QueryRow(
		"SELECT sender_jid, message_id FROM whatsmeow_message_secrets WHERE chat_jid = ? ORDER BY rowid DESC LIMIT 1",
		chatID,
	).Scan(&senderJID, &messageID)
	if err != nil || messageID == "" {
		return historyAnchor{}, false
	}
	fromMe := false
	if c := sm.client; c != nil && c.Store != nil && c.Store.ID != nil {
		if sender, parseErr := types.ParseJID(senderJID); parseErr == nil {
			fromMe = sender.User == c.Store.ID.User
		}
	}
	return historyAnchor{ID: messageID, FromMe: fromMe, Timestamp: uint64(time.Now().Unix())}, true
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func formatUnixDate(ts uint64) string {
	if ts == 0 {
		return "-"
	}
	return time.Unix(int64(ts), 0).Format("2006-01-02 15:04")
}

// RefetchHistory pages backwards through each chat's history on the phone
// and adds every message missing from the local cache. Chats are processed
// sequentially. The phone must be online. Cancelling ctx stops after the
// current request; caches are saved before returning either way.
func (sm *SessionManager) RefetchHistory(ctx context.Context, opts HistoryRefetchOptions) []HistoryRefetchResult {
	opts.applyDefaults()
	logf := opts.Logf
	results := make([]HistoryRefetchResult, 0, len(opts.ChatIDs))
	for i, rawID := range opts.ChatIDs {
		chatID := strings.TrimSpace(rawID)
		if chatID == "" {
			continue
		}
		if ctx.Err() != nil {
			results = append(results, HistoryRefetchResult{ChatID: chatID, StopReason: "interrupted", Err: ctx.Err()})
			continue
		}
		if i > 0 {
			if err := sleepCtx(ctx, opts.Delay); err != nil {
				results = append(results, HistoryRefetchResult{ChatID: chatID, StopReason: "interrupted", Err: err})
				continue
			}
		}
		result := sm.refetchChatHistory(ctx, chatID, opts)
		results = append(results, result)
		sm.SaveCaches()
		if result.Err != nil {
			logf("[refetch] %s done with error: pages=%d returned=%d added=%d oldest=%s reason=%q error=%v",
				chatID, result.Pages, result.Returned, result.Added, formatUnixDate(result.OldestTimestamp), result.StopReason, result.Err)
		} else {
			logf("[refetch] %s done: pages=%d returned=%d added=%d oldest=%s reason=%q",
				chatID, result.Pages, result.Returned, result.Added, formatUnixDate(result.OldestTimestamp), result.StopReason)
		}
	}
	return results
}

func (sm *SessionManager) refetchChatHistory(ctx context.Context, chatID string, opts HistoryRefetchOptions) HistoryRefetchResult {
	logf := opts.Logf
	result := HistoryRefetchResult{ChatID: chatID}
	candidates := sm.initialRefetchAnchors(chatID, refetchInitialAnchors)
	anchorless := len(candidates) == 0
	if anchorless {
		// Nothing cached for this chat. Try the newest message ID whatsmeow
		// kept a secret for (polls, etc.), then a blank anchor at "now" —
		// the phone's handling of an unknown anchor is undocumented, so
		// this is best effort and simply times out if ignored.
		if secret, ok := sm.messageSecretAnchor(chatID); ok {
			candidates = append(candidates, secret)
		}
		candidates = append(candidates, historyAnchor{Timestamp: uint64(time.Now().Unix())})
		logf("[refetch] %s start: no cached messages; trying %d fallback anchor(s) (best effort) since=%s", chatID, len(candidates), sinceLabel(opts.Since))
	} else {
		cachedBefore := len(sm.db.GetMessages(chatID))
		logf("[refetch] %s start: cached=%d newest=%s since=%s", chatID, cachedBefore, formatUnixDate(candidates[0].Timestamp), sinceLabel(opts.Since))
	}

	anchor := candidates[0]
	seenAnchors := map[string]bool{}
	var sinceUnix uint64
	if !opts.Since.IsZero() && opts.Since.Unix() > 0 {
		sinceUnix = uint64(opts.Since.Unix())
	}

	for page := 1; page <= opts.MaxPages; page++ {
		if page > 1 {
			if err := sleepCtx(ctx, opts.Delay); err != nil {
				result.StopReason = "interrupted"
				result.Err = err
				return result
			}
		}
		// The first page may fall back to older cached anchors; later pages
		// retry the same anchor once, since a single missed response is
		// usually just a slow phone (and a failed send usually a reconnect
		// the watchdog is about to repair).
		attempts := []historyAnchor{anchor, anchor}
		if page == 1 {
			attempts = candidates
		}
		var got historyPage
		var err error
		for attempt, candidate := range attempts {
			if attempt > 0 {
				logf("[refetch] %s page %d: %v; retrying (anchor=%s %s)", chatID, page, err, candidate.ID, formatUnixDate(candidate.Timestamp))
				pause := opts.Delay
				if !errors.Is(err, errHistoryTimeout) && pause < 10*time.Second {
					pause = 10 * time.Second
				}
				if sleepCtx(ctx, pause) != nil {
					break
				}
			}
			got, err = sm.requestHistoryPage(ctx, chatID, candidate, opts)
			if err == nil {
				anchor = candidate
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			result.StopReason = "interrupted"
			result.Err = ctx.Err()
			return result
		}
		if err != nil {
			if errors.Is(err, errHistoryTimeout) && page == 1 && anchorless {
				result.StopReason = "no cached message to anchor on"
				result.Err = errors.New("chat has no cached messages and the phone ignored fallback anchors; is this account a member? Open the chat on the phone/app or wait for a new message, then re-run")
			} else if errors.Is(err, errHistoryTimeout) {
				result.StopReason = fmt.Sprintf("no response within %s", opts.Timeout)
				// Past page 1 a silent phone most likely means the chat's
				// history simply ends there; on page 1 nothing worked at all.
				if page == 1 {
					result.Err = err
				}
			} else {
				result.StopReason = "request failed"
				result.Err = err
			}
			return result
		}

		result.Pages++
		result.Returned += got.Returned
		result.Added += got.Added
		if got.Returned > 0 && (result.OldestTimestamp == 0 || got.Oldest.Timestamp < result.OldestTimestamp) {
			result.OldestTimestamp = got.Oldest.Timestamp
		}
		logf("[refetch] %s page %d: returned=%d added=%d oldest=%s total_added=%d",
			chatID, page, got.Returned, got.Added, formatUnixDate(got.Oldest.Timestamp), result.Added)

		if page%opts.SaveEvery == 0 {
			sm.SaveCaches()
		}

		switch {
		case got.Returned == 0 || got.Oldest.ID == "":
			result.StopReason = "phone returned no older messages"
			return result
		case got.Oldest.ID == anchor.ID || got.Oldest.Timestamp > anchor.Timestamp || seenAnchors[got.Oldest.ID]:
			result.StopReason = "no progress past anchor"
			return result
		case sinceUnix > 0 && got.Oldest.Timestamp < sinceUnix:
			result.StopReason = "reached --since"
			return result
		}
		seenAnchors[anchor.ID] = true
		anchor = got.Oldest
	}
	result.StopReason = fmt.Sprintf("reached max pages (%d)", opts.MaxPages)
	return result
}

func sinceLabel(since time.Time) string {
	if since.IsZero() {
		return "none"
	}
	return since.Format("2006-01-02")
}
