package messages

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/adrg/xdg"
	"github.com/antong314/whatscli-rs/backend/config"
)

// TestMain points every cache path at a throwaway XDG config home. Some
// tests exercise code that saves the chat/message caches (directly or via
// the coalesced cache-refresh timer); they must never overwrite the real
// ~/Library/Application Support/whatscli files.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "whatscli-messages-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	xdg.Reload()
	if path := config.GetMessageCacheFilePath(); !strings.HasPrefix(path, dir) {
		fmt.Fprintf(os.Stderr, "refusing to run: message cache path %q is outside the test dir\n", path)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// noopUI is a UiMessageHandler that ignores everything.
type noopUI struct{}

func (noopUI) NewMessage(Message)                            {}
func (noopUI) NewTranslation(Message, string)                {}
func (noopUI) NewTranscription(Message, string)              {}
func (noopUI) NewScreen([]Message)                           {}
func (noopUI) SetChats([]Chat)                               {}
func (noopUI) PrintError(error)                              {}
func (noopUI) PrintText(string)                              {}
func (noopUI) PrintFile(string)                              {}
func (noopUI) FileSaved(string, string)                      {}
func (noopUI) MessageStatus(string, []string, MessageStatus) {}
func (noopUI) SetStatus(SessionStatus)                       {}
func (noopUI) OpenFile(string)                               {}
func (noopUI) GetWriter() io.Writer                          { return io.Discard }
func (noopUI) GetViewportLines() int                         { return 40 }
func (noopUI) QRCode(string)                                 {}
func (noopUI) QREvent(string)                                {}
func (noopUI) LoginSuccess()                                 {}
