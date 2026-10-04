package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antong314/whatscli-rs/backend/messages"
)

// refetchFlags holds the one-shot --refetch-history mode configuration.
type refetchFlags struct {
	chats    string
	since    string
	pageSize int
	timeout  time.Duration
	delay    time.Duration
	maxPages int
}

// parseRefetchSince accepts YYYY-MM-DD (local midnight) or RFC 3339.
func parseRefetchSince(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD)", value)
}

// parseRefetchChats splits a comma/space separated JID list, dropping
// blanks and duplicates while keeping order.
func parseRefetchChats(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" || seen[field] {
			continue
		}
		seen[field] = true
		out = append(out, field)
	}
	return out
}

// otherBackendRunning reports a live whatscli-server (e.g. the one the
// WhatsLingo app supervises). Two processes on one WhatsApp session fight
// over the connection and both rewrite messages.json, so refetch refuses
// to start next to one.
func otherBackendRunning() (string, bool) {
	if conn, err := net.DialTimeout("unix", socketPath(), time.Second); err == nil {
		conn.Close()
		return "a backend is listening on " + socketPath(), true
	}
	if data, err := os.ReadFile(pidPath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && pid != os.Getpid() {
			if syscall.Kill(pid, 0) == nil {
				return fmt.Sprintf("pid file %s points at running process %d", pidPath(), pid), true
			}
		}
	}
	if out, err := exec.Command("pgrep", "-x", "whatscli-server").Output(); err == nil {
		for _, line := range strings.Fields(string(out)) {
			if pid, err := strconv.Atoi(line); err == nil && pid != os.Getpid() {
				return fmt.Sprintf("whatscli-server process %d is running", pid), true
			}
		}
	}
	return "", false
}

// runRefetch connects with the stored session (no gRPC socket, no
// translation/transcription models), re-fetches history for the given
// chats, saves the caches and exits. Returns the process exit code.
func runRefetch(flags refetchFlags) int {
	chats := parseRefetchChats(flags.chats)
	if len(chats) == 0 {
		fmt.Fprintln(os.Stderr, "--refetch-history needs at least one chat JID")
		return 2
	}
	since, err := parseRefetchSince(flags.since)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if why, running := otherBackendRunning(); running {
		fmt.Fprintf(os.Stderr, "refusing to re-fetch: %s.\nQuit WhatsLingo (and any TUI-started backend) first; two connections on one session conflict.\n", why)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[refetch] interrupted — finishing the current request and saving…")
		cancel()
		<-sigCh
		fmt.Println("[refetch] second interrupt, exiting without waiting")
		os.Exit(130)
	}()

	sm := &messages.SessionManager{}
	sm.Init(consoleHandler{})
	if err := sm.StartManager(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start session manager: %v\n", err)
		return 1
	}
	fmt.Printf("[refetch] connecting and loading caches (chats=%d since=%s)…\n", len(chats), flags.since)
	if err := sm.WaitUntilReady(ctx, 3*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "[refetch] %v\n", err)
		sm.Shutdown()
		return 1
	}

	started := time.Now()
	results := sm.RefetchHistory(ctx, messages.HistoryRefetchOptions{
		ChatIDs:  chats,
		Since:    since,
		PageSize: flags.pageSize,
		Timeout:  flags.timeout,
		Delay:    flags.delay,
		MaxPages: flags.maxPages,
	})
	// Let any history-sync event still being processed land before the
	// final save.
	time.Sleep(time.Second)
	sm.Shutdown()

	exitCode := 0
	totalAdded := 0
	fmt.Printf("\n[refetch] summary (%s):\n", time.Since(started).Round(time.Second))
	for _, r := range results {
		totalAdded += r.Added
		oldest := "-"
		if r.OldestTimestamp > 0 {
			oldest = time.Unix(int64(r.OldestTimestamp), 0).Format("2006-01-02")
		}
		status := "ok"
		if r.Err != nil {
			status = "ERROR: " + r.Err.Error()
			exitCode = 1
		}
		fmt.Printf("  %-32s pages=%-4d returned=%-5d added=%-5d oldest=%s  %s (%s)\n",
			r.ChatID, r.Pages, r.Returned, r.Added, oldest, r.StopReason, status)
	}
	fmt.Printf("[refetch] added %d messages in total; caches saved.\n", totalAdded)
	if ctx.Err() != nil {
		return 130
	}
	return exitCode
}

// consoleHandler is the UiMessageHandler for headless refetch mode: status
// and errors go to the terminal, everything UI-specific is dropped.
type consoleHandler struct{}

func (consoleHandler) NewMessage(messages.Message)                            {}
func (consoleHandler) NewTranslation(messages.Message, string)                {}
func (consoleHandler) NewTranscription(messages.Message, string)              {}
func (consoleHandler) NewScreen([]messages.Message)                           {}
func (consoleHandler) SetChats([]messages.Chat)                               {}
func (consoleHandler) PrintError(err error)                                   { fmt.Fprintf(os.Stderr, "[session] error: %v\n", err) }
func (consoleHandler) PrintText(text string)                                  { fmt.Printf("[session] %s\n", text) }
func (consoleHandler) PrintFile(string)                                       {}
func (consoleHandler) FileSaved(string, string)                               {}
func (consoleHandler) MessageStatus(string, []string, messages.MessageStatus) {}
func (consoleHandler) SetStatus(messages.SessionStatus)                       {}
func (consoleHandler) OpenFile(string)                                        {}
func (consoleHandler) GetWriter() io.Writer                                   { return os.Stdout }
func (consoleHandler) GetViewportLines() int                                  { return 40 }
func (consoleHandler) QRCode(string) {
	fmt.Fprintln(os.Stderr, "[session] WhatsApp asked for QR pairing — this session is not logged in. Pair via the app first; aborting refetch.")
}
func (consoleHandler) QREvent(event string) { fmt.Printf("[session] QR event: %s\n", event) }
func (consoleHandler) LoginSuccess()        {}
