package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"time"

	"code.rocketnine.space/tslocum/cbind"
	"github.com/gdamore/tcell/v2"
	"github.com/antong314/whatscli-rs/backend/config"
	"github.com/antong314/whatscli-rs/backend/imagerender"
	"github.com/antong314/whatscli-rs/backend/messages"
	"github.com/antong314/whatscli-rs/backend/qrcode"
	"github.com/antong314/whatscli-rs/backend/transcribe"
	"github.com/antong314/whatscli-rs/backend/translate"
	"github.com/rivo/tview"
	"github.com/skratchdot/open-golang/open"
	"github.com/zyedidia/clipboard"
)

var VERSION string = "v1.1.3"

var sndTxt string = ""
var currentReceiver messages.Chat = messages.Chat{}
var curRegions []messages.Message

var textView *tview.TextView
var treeView *tview.TreeView
var textInput *tview.InputField
var topBar *tview.TextView
var infoBar *tview.TextView
var searchInput *tview.InputField
var pages *tview.Pages

var chatRoot *tview.TreeNode
var app *tview.Application
var searchActive bool

var sessionManager *messages.SessionManager

var keyBindings *cbind.Configuration

var uiHandler messages.UiMessageHandler

func main() {
	// tcell v2.3.11 only activates 24-bit color when it can find a built-in
	// terminfo entry AND COLORTERM is set.  Terminals like Ghostty set
	// TERM=xterm-ghostty which isn't in tcell's built-in database; the
	// dynamic infocmp fallback bypasses the COLORTERM augmentation path,
	// resulting in 256-color mode and washed-out/grayscale image rendering.
	// Fix: fall back to xterm-256color (which tcell knows) so the
	// COLORTERM=truecolor augmentation kicks in properly.
	if os.Getenv("COLORTERM") == "" {
		os.Setenv("COLORTERM", "truecolor")
	}
	term := os.Getenv("TERM")
	if term != "" && term != "xterm-256color" && term != "xterm" && term != "screen-256color" {
		os.Setenv("TERM", "xterm-256color")
	}

	config.InitConfig()
	uiHandler = UiHandler{}
	sessionManager = &messages.SessionManager{}
	sessionManager.Init(uiHandler)

	if config.Config.General.EnableTranslation {
		initTranslator()
	}
	initTranscriber()

	imagerender.CheckKitty()
	imagerender.CheckChafa()
	imagerender.LoadCache()

	app = tview.NewApplication()

	sideBarWidth := config.Config.Ui.ChatSidebarWidth
	gridLayout := tview.NewGrid()
	gridLayout.SetRows(1, 0, 1)
	gridLayout.SetColumns(sideBarWidth, 0, sideBarWidth)
	gridLayout.SetBorders(true)
	gridLayout.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	gridLayout.SetBordersColor(tcell.ColorNames[config.Config.Colors.Borders])

	cmdPrefix := config.Config.General.CmdPrefix
	topBar = tview.NewTextView()
	topBar.SetDynamicColors(true)
	topBar.SetScrollable(false)
	topBar.SetText("[::b] WhatsCLI " + VERSION + "  [-::d]Type " + cmdPrefix + "help or press " + config.Config.Keymap.CommandHelp + " for help")
	topBar.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])

	infoBar = tview.NewTextView()
	infoBar.SetDynamicColors(true)
	UpdateStatusBar(messages.SessionStatus{})

	textView = tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true).
		SetChangedFunc(func() {
			app.Draw()
		})
	textView.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	textView.SetTextColor(tcell.ColorNames[config.Config.Colors.Text])

	PrintHelp()

	textInput = tview.NewInputField()
	textInput.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	textInput.SetFieldBackgroundColor(tcell.ColorNames[config.Config.Colors.InputBackground])
	textInput.SetFieldTextColor(tcell.ColorNames[config.Config.Colors.InputText])
	textInput.SetChangedFunc(func(change string) {
		sndTxt = change
	})
	textInput.SetDoneFunc(EnterCommand)
	textInput.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyDown {
			offset, _ := textView.GetScrollOffset()
			offset += 1
			textView.ScrollTo(offset, 0)
			return nil
		}
		if event.Key() == tcell.KeyUp {
			offset, _ := textView.GetScrollOffset()
			offset -= 1
			textView.ScrollTo(offset, 0)
			return nil
		}
		if event.Key() == tcell.KeyPgDn {
			offset, _ := textView.GetScrollOffset()
			offset += 10
			textView.ScrollTo(offset, 0)
			return nil
		}
		if event.Key() == tcell.KeyPgUp {
			offset, _ := textView.GetScrollOffset()
			offset -= 10
			textView.ScrollTo(offset, 0)
			return nil
		}
		return event
	})

	searchInput = tview.NewInputField()
	searchInput.SetLabel("🔍 ")
	searchInput.SetFieldBackgroundColor(tcell.ColorNames[config.Config.Colors.InputBackground])
	searchInput.SetFieldTextColor(tcell.ColorNames[config.Config.Colors.InputText])
	searchInput.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])
	searchInput.SetChangedFunc(func(text string) {
		if text == "" {
			restoreChatTree()
		} else {
			filterChatTree(text)
		}
	})
	searchInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			selectFirstFilteredChat()
		}
		closeSearch()
	})
	searchInput.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEscape {
			closeSearch()
			return nil
		}
		return event
	})

	searchOverlay := tview.NewFlex().SetDirection(tview.FlexRow)
	searchOverlay.AddItem(nil, 0, 1, false)
	searchOverlay.AddItem(searchInput, 1, 0, true)

	gridLayout.AddItem(topBar, 0, 0, 1, 4, 0, 0, false)
	gridLayout.AddItem(infoBar, 2, 0, 1, 1, 0, 0, false)
	gridLayout.AddItem(MakeTree(), 1, 0, 1, 1, 0, 0, false)
	gridLayout.AddItem(textView, 1, 1, 1, 3, 0, 0, false)
	gridLayout.AddItem(textInput, 2, 1, 1, 3, 0, 0, false)

	pages = tview.NewPages()
	pages.AddPage("main", gridLayout, true, true)
	pages.AddPage("search", searchOverlay, true, false)

	app.SetRoot(pages, true)
	app.EnableMouse(true)
	app.SetFocus(textInput)
	if err := sessionManager.StartManager(); err != nil {
		PrintError(err)
	}
	LoadShortcuts()
	app.Run()
}

// creates the TreeView for chats
func MakeTree() *tview.TreeView {
	rootDir := "Chats"
	chatRoot = tview.NewTreeNode(rootDir).
		SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
	treeView = tview.NewTreeView().
		SetRoot(chatRoot).
		SetCurrentNode(chatRoot)
	treeView.SetBackgroundColor(tcell.ColorNames[config.Config.Colors.Background])

	treeView.SetChangedFunc(func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return
		}
		recv := reference.(messages.Chat)
		SetDisplayedChat(recv)
	})
	treeView.SetSelectedFunc(func(node *tview.TreeNode) {
		children := node.GetChildren()
		if len(children) > 0 {
			node.SetExpanded(!node.IsExpanded())
		}
	})
	return treeView
}

func handleFocusMessage(ev *tcell.EventKey) *tcell.EventKey {
	if !textView.HasFocus() {
		app.SetFocus(textView)
		if curRegions != nil && len(curRegions) > 0 {
			textView.Highlight(curRegions[len(curRegions)-1].Id)
		}
	}
	return nil
}

func handleFocusInput(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !textInput.HasFocus() {
		app.SetFocus(textInput)
	}
	return nil
}

func handleFocusContacts(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !treeView.HasFocus() {
		app.SetFocus(treeView)
	}
	return nil
}

func handleSwitchPanels(ev *tcell.EventKey) *tcell.EventKey {
	ResetMsgSelection()
	if !textInput.HasFocus() {
		app.SetFocus(textInput)
	} else {
		app.SetFocus(treeView)
	}
	return nil
}

func handleCommand(command string) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		sessionManager.CommandChannel <- messages.Command{command, nil}
		return nil
	}
}

func handleCopyUser(ev *tcell.EventKey) *tcell.EventKey {
	if hls := textView.GetHighlights(); len(hls) > 0 {
		for _, val := range curRegions {
			if val.Id == hls[0] {
				clipboard.WriteAll(val.ContactId, "clipboard")
				PrintText("copied id of " + val.ContactName + " to clipboard")
			}
		}
		ResetMsgSelection()
	} else if currentReceiver.Id != "" {
		clipboard.WriteAll(currentReceiver.Id, "clipboard")
		PrintText("copied id of " + currentReceiver.Name + " to clipboard")
	}
	return nil
}

func handlePasteUser(ev *tcell.EventKey) *tcell.EventKey {
	if clip, err := safeReadClipboard(); err == nil {
		textInput.SetText(textInput.GetText() + " " + clip)
	} else {
		PrintError(err)
	}
	return nil
}

func safeReadClipboard() (clip string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("clipboard paste is unavailable: %v", rec)
		}
	}()
	return clipboard.ReadAll("clipboard")
}

func handleQuit(ev *tcell.EventKey) *tcell.EventKey {
	sessionManager.CommandChannel <- messages.Command{"disconnect", nil}
	app.Stop()
	return nil
}

func handleHelp(ev *tcell.EventKey) *tcell.EventKey {
	PrintHelp()
	return nil
}

func handleMessageCommand(command string) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		hls := textView.GetHighlights()
		if len(hls) > 0 {
			sessionManager.CommandChannel <- messages.Command{command, []string{hls[0]}}
			ResetMsgSelection()
			app.SetFocus(textInput)
		}
		return nil
	}
}

func handleMessagesMove(amount int) func(ev *tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		if curRegions == nil || len(curRegions) == 0 {
			return nil
		}
		hls := textView.GetHighlights()
		if len(hls) > 0 {
			newId := GetOffsetMsgId(hls[0], amount)
			if newId != "" {
				textView.Highlight(newId)
			}
		} else {
			if amount < 0 {
				textView.Highlight(curRegions[0].Id)
			} else {
				textView.Highlight(curRegions[len(curRegions)-1].Id)
			}
		}
		textView.ScrollToHighlight()
		return nil
	}
}

func handleJumpUnread(ev *tcell.EventKey) *tcell.EventKey {
	chats := sessionManager.GetChats()
	if len(chats) == 0 {
		return nil
	}
	startIdx := -1
	for i, c := range chats {
		if c.Id == currentReceiver.Id {
			startIdx = i
			break
		}
	}
	for offset := 1; offset <= len(chats); offset++ {
		idx := (startIdx + offset) % len(chats)
		if chats[idx].Unread > 0 {
			selectChatByID(chats[idx].Id)
			return nil
		}
	}
	PrintText("[::d]no unread chats[::-]")
	return nil
}

func selectChatByID(id string) {
	children := chatRoot.GetChildren()
	for _, node := range children {
		ref := node.GetReference()
		if ref == nil {
			for _, child := range node.GetChildren() {
				cref := child.GetReference()
				if cref == nil {
					continue
				}
				chat := cref.(messages.Chat)
				if chat.Id == id {
					node.SetExpanded(true)
					treeView.SetCurrentNode(child)
					SetDisplayedChat(chat)
					return
				}
			}
			continue
		}
		chat := ref.(messages.Chat)
		if chat.Id == id {
			treeView.SetCurrentNode(node)
			SetDisplayedChat(chat)
			return
		}
	}
}

func handleSearchChats(ev *tcell.EventKey) *tcell.EventKey {
	if searchActive {
		return nil
	}
	searchActive = true
	searchInput.SetText("")
	pages.ShowPage("search")
	app.SetFocus(searchInput)
	return nil
}

func closeSearch() {
	searchActive = false
	pages.HidePage("search")
	app.SetFocus(textInput)
	restoreChatTree()
}

func restoreChatTree() {
	chats := sessionManager.GetChats()
	oldId := currentReceiver.Id
	chatRoot.ClearChildren()

	var archivedChats, regularChats []messages.Chat
	for _, element := range chats {
		if element.Archived {
			archivedChats = append(archivedChats, element)
		} else {
			regularChats = append(regularChats, element)
		}
	}

	if len(archivedChats) > 0 {
		archivedFolder := tview.NewTreeNode(fmt.Sprintf("Archived (%d)", len(archivedChats))).
			SetSelectable(true).
			SetExpanded(false).
			SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
		for _, element := range archivedChats {
			node := makeChatNode(element)
			archivedFolder.AddChild(node)
			if element.Id == oldId {
				treeView.SetCurrentNode(node)
			}
		}
		chatRoot.AddChild(archivedFolder)
	}

	for _, element := range regularChats {
		node := makeChatNode(element)
		chatRoot.AddChild(node)
		if element.Id == oldId {
			treeView.SetCurrentNode(node)
		}
	}
}

func filterChatTree(query string) {
	query = strings.ToLower(query)
	chats := sessionManager.GetChats()
	chatRoot.ClearChildren()
	for _, element := range chats {
		name := element.Name
		if name == "" {
			name = strings.TrimSuffix(strings.TrimSuffix(element.Id, messages.GROUPSUFFIX), messages.CONTACTSUFFIX)
		}
		if !strings.Contains(strings.ToLower(name), query) {
			continue
		}
		node := makeChatNode(element)
		chatRoot.AddChild(node)
	}
	children := chatRoot.GetChildren()
	if len(children) > 0 {
		treeView.SetCurrentNode(children[0])
	}
}

func selectFirstFilteredChat() {
	children := chatRoot.GetChildren()
	if len(children) > 0 {
		ref := children[0].GetReference()
		if ref != nil {
			chat := ref.(messages.Chat)
			treeView.SetCurrentNode(children[0])
			SetDisplayedChat(chat)
		}
	}
}

func handleChatPanelUp(ev *tcell.EventKey) *tcell.EventKey {
	return ev
}

func handleChatPanelDown(ev *tcell.EventKey) *tcell.EventKey {
	return ev
}

func handleMessagesLast(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	textView.Highlight(curRegions[len(curRegions)-1].Id)
	textView.ScrollToHighlight()
	return nil
}

func handleMessagesFirst(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	textView.Highlight(curRegions[0].Id)
	textView.ScrollToHighlight()
	return nil
}

func handleExitMessages(ev *tcell.EventKey) *tcell.EventKey {
	if curRegions == nil || len(curRegions) == 0 {
		return nil
	}
	ResetMsgSelection()
	app.SetFocus(textInput)
	return nil
}

// load the key map
func LoadShortcuts() {
	// global bindings for app
	keyBindings = cbind.NewConfiguration()
	if err := keyBindings.Set(config.Config.Keymap.FocusMessages, handleFocusMessage); err != nil {
		PrintErrorMsg("focus_messages:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.FocusInput, handleFocusInput); err != nil {
		PrintErrorMsg("focus_input:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.FocusChats, handleFocusContacts); err != nil {
		PrintErrorMsg("focus_contacts:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.SwitchPanels, handleSwitchPanels); err != nil {
		PrintErrorMsg("switch_panels:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandRead, handleCommand("read")); err != nil {
		PrintErrorMsg("command_read:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandUnread, handleCommand("unread")); err != nil {
		PrintErrorMsg("command_unread:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.Copyuser, handleCopyUser); err != nil {
		PrintErrorMsg("copyuser:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.Pasteuser, handlePasteUser); err != nil {
		PrintErrorMsg("pasteuser:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandBacklog, handleCommand("backlog")); err != nil {
		PrintErrorMsg("command_backlog:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandConnect, handleCommand("login")); err != nil {
		PrintErrorMsg("command_connect:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandQuit, handleQuit); err != nil {
		PrintErrorMsg("command_quit:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.CommandHelp, handleHelp); err != nil {
		PrintErrorMsg("command_help:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.JumpUnread, handleJumpUnread); err != nil {
		PrintErrorMsg("jump_unread:", err)
	}
	if err := keyBindings.Set(config.Config.Keymap.SearchChats, handleSearchChats); err != nil {
		PrintErrorMsg("search_chats:", err)
	}
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if searchActive {
			return event
		}
		return keyBindings.Capture(event)
	})
	// bindings for chat message text view
	keysMessages := cbind.NewConfiguration()
	if err := keysMessages.Set(config.Config.Keymap.MessageDownload, handleMessageCommand("download")); err != nil {
		PrintErrorMsg("message_download:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageOpen, handleMessageCommand("open")); err != nil {
		PrintErrorMsg("message_open:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.Copyuser, handleCopyUser); err != nil {
		PrintErrorMsg("copyuser:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.Pasteuser, handlePasteUser); err != nil {
		PrintErrorMsg("pasteuser:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageShow, handleMessageCommand("show")); err != nil {
		PrintErrorMsg("message_show:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageUrl, handleMessageCommand("url")); err != nil {
		PrintErrorMsg("message_url:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageInfo, handleMessageCommand("info")); err != nil {
		PrintErrorMsg("message_info:", err)
	}
	if err := keysMessages.Set(config.Config.Keymap.MessageRevoke, handleMessageCommand("revoke")); err != nil {
		PrintErrorMsg("message_revoke:", err)
	}
	keysMessages.SetKey(tcell.ModNone, tcell.KeyEscape, handleExitMessages)
	keysMessages.SetKey(tcell.ModNone, tcell.KeyUp, handleMessagesMove(-1))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyDown, handleMessagesMove(1))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyPgUp, handleMessagesMove(-10))
	keysMessages.SetKey(tcell.ModNone, tcell.KeyPgDn, handleMessagesMove(10))
	keysMessages.SetRune(tcell.ModNone, 'k', handleMessagesMove(-1))
	keysMessages.SetRune(tcell.ModNone, 'j', handleMessagesMove(1))
	keysMessages.SetRune(tcell.ModNone, 'g', handleMessagesFirst)
	keysMessages.SetRune(tcell.ModNone, 'G', handleMessagesLast)
	keysMessages.SetRune(tcell.ModCtrl, 'u', handleMessagesMove(-10))
	keysMessages.SetRune(tcell.ModCtrl, 'd', handleMessagesMove(10))
	textView.SetInputCapture(keysMessages.Capture)
	keysChatPanel := cbind.NewConfiguration()
	keysChatPanel.SetRune(tcell.ModCtrl, 'u', handleChatPanelUp)
	keysChatPanel.SetRune(tcell.ModCtrl, 'd', handleChatPanelDown)
	treeView.SetInputCapture(keysChatPanel.Capture)
}

// prints help to chat view
func PrintHelp() {
	cmdPrefix := config.Config.General.CmdPrefix
	fmt.Fprintln(textView, "[-::u]Keys:[-::-]")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Global")
	fmt.Fprintln(textView, "[::b] Up/Down[::-] = Scroll history/chats")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.SwitchPanels, "[::-] = Switch input/chats")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.FocusMessages, "[::-] = Focus message panel")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.SearchChats, "[::-] = Search chats")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.JumpUnread, "[::-] = Jump to next unread chat")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.CommandRead, "[::-] = Mark chat as read")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.CommandUnread, "[::-] = Mark chat as unread")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.CommandBacklog, "[::-] = Load previous messages")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.CommandQuit, "[::-] = Exit app")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Message panel[-::-]")
	fmt.Fprintln(textView, "[::b] Up/Down[::-] = select message")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageDownload, "[::-] = Download attachment")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageOpen, "[::-] = Download & open attachment")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageShow, "[::-] = Download & show image using", config.Config.General.ShowCommand)
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageUrl, "[::-] = Find URL in message and open it")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageRevoke, "[::-] = Revoke message")
	fmt.Fprintln(textView, "[::b]", config.Config.Keymap.MessageInfo, "[::-] = Info about message")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Config file in ->", config.GetConfigFilePath())
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Type [::b]"+cmdPrefix+"commands[::-] to see all commands")
	fmt.Fprintln(textView, "")
}

func PrintCommands() {
	cmdPrefix := config.Config.General.CmdPrefix
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::u]Commands:[-::-]")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Global[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"connect [::-]or[::b]", config.Config.Keymap.CommandConnect, "[::-] = (Re)Connect to server")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"disconnect[::-]  = Close the connection")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"logout[::-]  = Remove login data from computer")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"reset[::-]  = Remove stored session and reconnect cleanly")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"quit [::-]or[::b]", config.Config.Keymap.CommandQuit, "[::-] = Exit app")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Chat[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"backlog [::-]or[::b]", config.Config.Keymap.CommandBacklog, "[::-] = load next", config.Config.General.BacklogMsgQuantity, "previous messages")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"read [::-]or[::b]", config.Config.Keymap.CommandRead, "[::-] = mark new messages in chat as read")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"unread [::-]or[::b]", config.Config.Keymap.CommandUnread, "[::-] = mark chat as unread")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"upload[::-] /path/to/file  = Upload any file as document")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendimage[::-] /path/to/file  = Send image message")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendvideo[::-] /path/to/file  = Send video message")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"sendaudio[::-] /path/to/file  = Send audio message")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Translation[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"translate[::-]  = Toggle auto-translation on/off")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"translate on[::-]  = Enable auto-translation")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"translate off[::-]  = Disable auto-translation")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "[-::-]Groups[-::-]")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"leave[::-]  = Leave group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"create[::-] [user-id[] [user-id[] Group Subject  = Create group with users")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"subject[::-] New Subject  = Change subject of group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"add[::-] [user-id[]  = Add user to group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"remove[::-] [user-id[]  = Remove user from group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"admin[::-] [user-id[]  = Set admin role for user in group")
	fmt.Fprintln(textView, "[::b] "+cmdPrefix+"removeadmin[::-] [user-id[]  = Remove admin role for user in group")
	fmt.Fprintln(textView, "")
	fmt.Fprintln(textView, "Use[::b]", config.Config.Keymap.Copyuser, "[::-]to copy a selected user id to clipboard")
	fmt.Fprintln(textView, "Use[::b]", config.Config.Keymap.Pasteuser, "[::-]to paste clipboard to text input")
	fmt.Fprintln(textView, "")
}

// called when text is entered by the user
func EnterCommand(key tcell.Key) {
	if sndTxt == "" {
		return
	}
	if key == tcell.KeyEsc {
		textInput.SetText("")
		return
	}
	cmdPrefix := config.Config.General.CmdPrefix
	if sndTxt == cmdPrefix+"help" {
		PrintHelp()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"commands" {
		PrintCommands()
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"translate" || strings.HasPrefix(sndTxt, cmdPrefix+"translate ") {
		handleTranslateCommand(strings.TrimPrefix(sndTxt, cmdPrefix+"translate"))
		textInput.SetText("")
		return
	}
	if sndTxt == cmdPrefix+"quit" {
		sessionManager.CommandChannel <- messages.Command{"disconnect", nil}
		app.Stop()
		return
	}
	if strings.HasPrefix(sndTxt, cmdPrefix) {
		cmd := strings.TrimPrefix(sndTxt, cmdPrefix)
		var params []string
		if strings.Index(cmd, " ") >= 0 {
			cmdParts := strings.Split(cmd, " ")
			cmd = cmdParts[0]
			params = cmdParts[1:]
		}
		sessionManager.CommandChannel <- messages.Command{cmd, params}
		textInput.SetText("")
		return
	}
	if currentReceiver.Id == "" {
		PrintText("no receiver")
		textInput.SetText("")
		return
	}
	// no command, send as message
	msg := messages.Command{
		Name:   "send",
		Params: []string{currentReceiver.Id, sndTxt},
	}
	sessionManager.CommandChannel <- msg
	textInput.SetText("")
}

// get the next message id to select (highlighted + offset)
func GetOffsetMsgId(curId string, offset int) string {
	if curRegions == nil || len(curRegions) == 0 {
		return ""
	}
	for idx, val := range curRegions {
		if val.Id == curId {
			arrPos := idx + offset
			if len(curRegions) > arrPos && arrPos >= 0 {
				return curRegions[arrPos].Id
			}
		}
	}
	if offset > 0 {
		return curRegions[0].Id
	} else {
		return curRegions[len(curRegions)-1].Id
	}
}

// resets the selection in the textView and scrolls it down
func ResetMsgSelection() {
	if len(textView.GetHighlights()) > 0 {
		textView.Highlight("")
	}
	textView.ScrollToEnd()
}

// prints text to the TextView
func PrintText(txt string) {
	fmt.Fprintln(textView, txt)
}

// prints an error to the TextView
func PrintError(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(textView, "["+config.Config.Colors.Negative+"]", err.Error(), "[-]")
}

// prints an error to the TextView
func PrintErrorMsg(text string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(textView, "["+config.Config.Colors.Negative+"]", text, err.Error(), "[-]")
}

// prints an image attachment to the TextView (by message id)
func PrintImage(path string) {
	var err error
	cmdParts := strings.Split(config.Config.General.ShowCommand, " ")
	cmdParts = append(cmdParts, path)
	var cmd *exec.Cmd
	size := len(cmdParts)
	if size > 1 {
		cmd = exec.Command(cmdParts[0], cmdParts[1:]...)
	} else if size > 0 {
		cmd = exec.Command(cmdParts[0])
	}
	var stdout io.ReadCloser
	if stdout, err = cmd.StdoutPipe(); err == nil {
		if err = cmd.Start(); err == nil {
			reader := bufio.NewReader(stdout)
			io.Copy(tview.ANSIWriter(textView), reader)
			return
		}
	}
	PrintError(err)
}

// updates the status bar
func UpdateStatusBar(statusInfo messages.SessionStatus) {
	out := " "
	if statusInfo.Connected {
		out += "[" + config.Config.Colors.Positive + "]online[-]"
	} else {
		out += "[" + config.Config.Colors.Negative + "]offline[-]"
	}
	out += " "
	out += "[::d] ("
	out += fmt.Sprint(statusInfo.BatteryCharge)
	out += "%"
	if statusInfo.BatteryLoading {
		out += " [" + config.Config.Colors.Positive + "]L[-]"
	} else {
		out += " [" + config.Config.Colors.Negative + "]l[-]"
	}
	if statusInfo.BatteryPowersave {
		out += " [" + config.Config.Colors.Negative + "]S[-]"
	} else {
		out += " [" + config.Config.Colors.Positive + "]s[-]"
	}
	out += ")[::-] "
	out += statusInfo.LastSeen
	infoBar.SetText(out)
	//infoBar.SetText("🔋: ??%")
}

// sets the current chat, loads text from storage to TextView
func SetDisplayedChat(wid messages.Chat) {
	if imagerender.IsKittyCapable() {
		imagerender.ClearAllKittyPlacements()
	}
	currentReceiver = wid
	textView.Clear()
	textView.SetTitle(wid.Name)
	sessionManager.CommandChannel <- messages.Command{"select", []string{currentReceiver.Id}}
}

// maxDisplayMessages limits how many messages are rendered in the chat view.
// Older messages can be loaded via Ctrl+b (backlog).
const maxDisplayMessages = 100

func getMessagesString(msgs []messages.Message) string {
	start := 0
	if len(msgs) > maxDisplayMessages {
		start = len(msgs) - maxDisplayMessages
	}
	var b strings.Builder
	b.Grow(len(msgs) * 160)
	for i := start; i < len(msgs); i++ {
		b.WriteString(getTextMessageString(&msgs[i]))
		b.WriteByte('\n')
		if msgs[i].Kind == messages.MessageKindImage {
			if ci, ok := imagerender.GetCached(msgs[i].Id); ok && ci.RenderedArt != "" {
				b.WriteString(ci.RenderedArt)
				b.WriteByte('\n')
			}
		}
		hasTranscript := false
		if st, ok := sessionManager.GetTranscription(msgs[i].Id); ok {
			b.WriteString(getTranscriptionString(&msgs[i], st))
			b.WriteByte('\n')
			hasTranscript = true
		}
		if tr, ok := sessionManager.GetTranslation(msgs[i].Id); ok {
			if msgs[i].Kind != messages.MessageKindAudio || hasTranscript {
				b.WriteString(getTranslationString(&msgs[i], tr))
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// create a formatted string with regions based on message ID from a text message
//TODO: optimize, use Sprintf etc
func getTextMessageString(msg *messages.Message) string {
	colorMe := config.Config.Colors.ChatMe
	colorContact := config.Config.Colors.ChatContact
	text := tview.Escape(msg.Text)
	if msg.Forwarded {
		text = "[" + config.Config.Colors.ForwardedText + "]" + text + "[-]"
	}
	ts := time.Unix(int64(msg.Timestamp), 0).Format("02-01-06 15:04:05")

	var b strings.Builder
	b.Grow(len(msg.Id) + len(text) + 80)
	b.WriteString("[\"")
	b.WriteString(msg.Id)
	b.WriteString("\"]")
	if msg.FromMe {
		b.WriteString("[-::d](")
		b.WriteString(ts)
		b.WriteString(") [")
		b.WriteString(colorMe)
		b.WriteString("::b]Me: [-::-]")
		b.WriteString(text)
	} else {
		senderLabel := msg.ContactShort
		if senderLabel == "" || strings.HasPrefix(senderLabel, "+") || strings.HasSuffix(senderLabel, "@lid") {
			senderLabel = msg.ContactName
		}
		if senderLabel == "" || strings.HasSuffix(senderLabel, "@lid") {
			senderLabel = msg.ContactShort
		}
		b.WriteString("[-::d](")
		b.WriteString(ts)
		b.WriteString(") [")
		b.WriteString(colorContact)
		b.WriteString("::b]")
		b.WriteString(senderLabel)
		b.WriteString(": [-::-]")
		b.WriteString(text)
	}
	b.WriteString("[\"\"]")
	return b.String()
}

type UiHandler struct{}

func (u UiHandler) NewMessage(msg messages.Message) {
	go app.QueueUpdateDraw(func() {
		curRegions = append(curRegions, msg)
		PrintText(getTextMessageString(&msg))
		renderSingleImage(msg)
	})
}

func (u UiHandler) NewTranslation(msg messages.Message, translatedText string) {
	sessionManager.StoreTranslation(msg.Id, translatedText)
	go app.QueueUpdateDraw(func() {
		if msg.ChatId != currentReceiver.Id {
			return
		}
		rebuildScreen()
	})
}

func (u UiHandler) NewTranscription(msg messages.Message, transcribedText string) {
	sessionManager.StoreTranscription(msg.Id, transcribedText)
	go app.QueueUpdateDraw(func() {
		if msg.ChatId != currentReceiver.Id {
			return
		}
		rebuildScreen()
	})
}

func (u UiHandler) NewScreen(msgs []messages.Message) {
	go app.QueueUpdateDraw(func() {
		textView.Clear()
		screen := getMessagesString(msgs)
		textView.SetText(screen)
		start := 0
		if len(msgs) > maxDisplayMessages {
			start = len(msgs) - maxDisplayMessages
		}
		curRegions = msgs[start:]
		if screen == "" {
			if currentReceiver.Id == "" {
				PrintHelp()
			} else {
				PrintText("[::d] ~~~ no messages, press " + config.Config.Keymap.CommandBacklog + " to load backlog if available ~~~[::-]")
			}
		}
		renderScreenImages(curRegions)
	})
}

func rebuildScreen() {
	if curRegions == nil || len(curRegions) == 0 {
		return
	}
	screen := getMessagesString(curRegions)
	textView.SetText(screen)
}

func getImageWidthCols() int {
	_, _, w, _ := textView.GetInnerRect()
	if w <= 0 {
		w = 80
	}
	if w < 20 {
		w = 20
	}
	return w
}

func getImageHeightRows() int {
	w := getImageWidthCols()
	// Images are typically 4:3 or 3:2 aspect ratio. Each terminal cell is
	// roughly 2:1 (width:height in pixels), so to approximate a square image
	// region we want rows ≈ cols/2. Cap to avoid consuming too much screen.
	h := w / 2
	if h < 10 {
		h = 10
	}
	if h > 30 {
		h = 30
	}
	return h
}

func renderScreenImages(msgs []messages.Message) {
	if !imagerender.IsAvailable() {
		return
	}
	chatID := currentReceiver.Id
	widthCols := getImageWidthCols()

	var toRender []messages.Message
	for i := len(msgs) - 1; i >= 0 && len(toRender) < 5; i-- {
		if msgs[i].Kind == messages.MessageKindImage {
			if _, ok := imagerender.GetCached(msgs[i].Id); !ok {
				toRender = append(toRender, msgs[i])
			}
		}
	}
	if len(toRender) == 0 {
		return
	}

	heightRows := getImageHeightRows()
	go func() {
		rendered := 0
		for _, msg := range toRender {
			path, err := sessionManager.DownloadImage(msg)
			if err != nil {
				continue
			}
			ci := imagerender.CachedImage{
				FilePath:        path,
				PlaceholderRows: heightRows,
			}
			if !imagerender.IsKittyCapable() {
				art, err := imagerender.RenderImage(path, widthCols)
				if err != nil {
					continue
				}
				ci.RenderedArt = art
			}
			imagerender.Store(msg.Id, ci)
			rendered++
		}
		if rendered > 0 {
			imagerender.SaveCache()
			app.QueueUpdateDraw(func() {
				if currentReceiver.Id != chatID {
					return
				}
				rebuildScreen()
				if imagerender.IsKittyCapable() {
					displayKittyImagesForChat()
				}
			})
		}
	}()
}

func renderSingleImage(msg messages.Message) {
	if !imagerender.IsAvailable() {
		return
	}
	if msg.Kind != messages.MessageKindImage {
		return
	}
	if _, ok := imagerender.GetCached(msg.Id); ok {
		return
	}
	chatID := msg.ChatId
	widthCols := getImageWidthCols()
	heightRows := getImageHeightRows()
	go func() {
		path, err := sessionManager.DownloadImage(msg)
		if err != nil {
			return
		}
		ci := imagerender.CachedImage{
			FilePath:        path,
			PlaceholderRows: heightRows,
		}
		if !imagerender.IsKittyCapable() {
			art, err := imagerender.RenderImage(path, widthCols)
			if err != nil {
				return
			}
			ci.RenderedArt = art
		}
		imagerender.Store(msg.Id, ci)
		imagerender.SaveCache()
		app.QueueUpdateDraw(func() {
			if currentReceiver.Id != chatID {
				return
			}
			rebuildScreen()
			if imagerender.IsKittyCapable() {
				displayKittyImagesForChat()
			}
		})
	}()
}

// displayKittyImagesForChat renders the most recent image in the current
// chat directly via the Kitty graphics protocol.  The image is placed at
// a fixed position inside the textView and persists across tcell redraws.
func displayKittyImagesForChat() {
	if curRegions == nil {
		return
	}

	x, y, w, h := textView.GetInnerRect()
	if w <= 0 || h <= 0 {
		return
	}

	imagerender.ClearAllKittyPlacements()

	widthCols := w
	heightRows := getImageHeightRows()
	if heightRows > h-2 {
		heightRows = h - 2
	}

	// Find the last image message
	for i := len(curRegions) - 1; i >= 0; i-- {
		if curRegions[i].Kind != messages.MessageKindImage {
			continue
		}
		ci, ok := imagerender.GetCached(curRegions[i].Id)
		if !ok || ci.FilePath == "" {
			continue
		}
		// Prepare PNG if not already done
		if !imagerender.HasPreparedPNG(curRegions[i].Id) {
			if err := imagerender.PrepareKittyImage(curRegions[i].Id, ci.FilePath, widthCols, heightRows); err != nil {
				continue
			}
		}
		// Display at top of text area (below the first message line)
		imagerender.DisplayKittyImage(curRegions[i].Id, x, y+1, widthCols, heightRows)
		break
	}
}

func makeChatNode(element messages.Chat) *tview.TreeNode {
	name := element.Name
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(element.Id, messages.GROUPSUFFIX), messages.CONTACTSUFFIX)
	}
	if element.Unread > 0 {
		name += " ([" + config.Config.Colors.UnreadCount + "]" + fmt.Sprint(element.Unread) + "[-])"
	}
	node := tview.NewTreeNode(name).
		SetReference(element).
		SetSelectable(true)
	if element.IsGroup {
		node.SetColor(tcell.ColorNames[config.Config.Colors.ListGroup])
	} else {
		node.SetColor(tcell.ColorNames[config.Config.Colors.ListContact])
	}
	return node
}

// loads the chat data from storage to the TreeView
func (u UiHandler) SetChats(ids []messages.Chat) {
	go app.QueueUpdateDraw(func() {
		chatRoot.ClearChildren()
		oldId := currentReceiver.Id

		var archivedChats, regularChats []messages.Chat
		for _, element := range ids {
			if element.Archived {
				archivedChats = append(archivedChats, element)
			} else {
				regularChats = append(regularChats, element)
			}
		}

		if len(archivedChats) > 0 {
			archivedFolder := tview.NewTreeNode(fmt.Sprintf("Archived (%d)", len(archivedChats))).
				SetSelectable(true).
				SetExpanded(false).
				SetColor(tcell.ColorNames[config.Config.Colors.ListHeader])
			for _, element := range archivedChats {
				node := makeChatNode(element)
				if element.Id == oldId {
					currentReceiver = element
				}
				archivedFolder.AddChild(node)
				if element.Id == currentReceiver.Id {
					treeView.SetCurrentNode(node)
				}
			}
			chatRoot.AddChild(archivedFolder)
		}

		for _, element := range regularChats {
			node := makeChatNode(element)
			if element.Id == oldId {
				currentReceiver = element
			}
			chatRoot.AddChild(node)
			if element.Id == currentReceiver.Id {
				treeView.SetCurrentNode(node)
			}
		}
	})
}

func (u UiHandler) PrintError(err error) {
	PrintError(err)
}

func (u UiHandler) PrintText(msg string) {
	PrintText(msg)
}

func (u UiHandler) PrintFile(path string) {
	go app.QueueUpdateDraw(func() {
		PrintImage(path)
	})
}

func (u UiHandler) OpenFile(path string) {
	open.Run(path)
}

func (u UiHandler) SetStatus(status messages.SessionStatus) {
	go app.QueueUpdateDraw(func() {
		UpdateStatusBar(status)
	})
}

func (u UiHandler) GetWriter() io.Writer {
	return textView
}

func (u UiHandler) GetViewportLines() int {
	_, _, _, h := textView.GetInnerRect()
	return h
}

func (u UiHandler) QRCode(code string) {
	terminal := qrcode.New()
	terminal.SetOutput(tview.ANSIWriter(textView))
	terminal.Get(code).Print()
}

func (u UiHandler) QREvent(event string) {
	PrintText("QR event: " + event)
}

func (u UiHandler) LoginSuccess() {
	PrintText("Successfully logged in!")
}

func getTranslationString(msg *messages.Message, translatedText string) string {
	colorTranslation := config.Config.Colors.TranslationText
	text := tview.Escape(translatedText)

	senderLabel := msg.ContactShort
	if senderLabel == "" || strings.HasPrefix(senderLabel, "+") || strings.HasSuffix(senderLabel, "@lid") {
		senderLabel = msg.ContactName
	}
	if senderLabel == "" || strings.HasSuffix(senderLabel, "@lid") {
		senderLabel = msg.ContactShort
	}
	if msg.FromMe {
		senderLabel = "Me"
	}

	var b strings.Builder
	b.Grow(len(text) + 80)
	b.WriteString("[")
	b.WriteString(colorTranslation)
	b.WriteString("]")
	b.WriteString("                    ")
	b.WriteString(senderLabel)
	b.WriteString("(EN): ")
	b.WriteString(text)
	b.WriteString("[-]")
	return b.String()
}

func getTranscriptionString(msg *messages.Message, transcribedText string) string {
	colorTranslation := config.Config.Colors.TranslationText
	text := tview.Escape(transcribedText)

	senderLabel := msg.ContactShort
	if senderLabel == "" || strings.HasPrefix(senderLabel, "+") || strings.HasSuffix(senderLabel, "@lid") {
		senderLabel = msg.ContactName
	}
	if senderLabel == "" || strings.HasSuffix(senderLabel, "@lid") {
		senderLabel = msg.ContactShort
	}
	if msg.FromMe {
		senderLabel = "Me"
	}

	var b strings.Builder
	b.Grow(len(text) + 80)
	b.WriteString("[")
	b.WriteString(colorTranslation)
	b.WriteString("]")
	b.WriteString("                    ")
	b.WriteString(senderLabel)
	b.WriteString("(STT): ")
	b.WriteString(text)
	b.WriteString("[-]")
	return b.String()
}

func initTranslator() {
	translate.SuppressStderr()
	t := translate.New(config.Config.General.TranslationDialect)
	sessionManager.Translator = t

	go func() {
		modelPath := config.Config.General.TranslationModelPath
		var lastPct int
		err := t.Init(modelPath, func(downloaded, total int64) {
			if total > 0 {
				pct := int(float64(downloaded) / float64(total) * 100)
				if pct != lastPct {
					lastPct = pct
					msg := fmt.Sprintf(" [::d]Downloading translation model: %d%%[::-]", pct)
					go app.QueueUpdateDraw(func() {
						topBar.SetText(msg)
					})
				}
			}
		})
		if err != nil {
			go app.QueueUpdateDraw(func() {
				topBar.SetText("[::b] WhatsCLI " + VERSION + "[-::-]")
				PrintText("[" + config.Config.Colors.Negative + "]Translation model failed to load: " + err.Error() + "[-]")
			})
			return
		}
		go app.QueueUpdateDraw(func() {
			topBar.SetText("[::b] WhatsCLI " + VERSION + "[-::-]")
			PrintText("[" + config.Config.Colors.Positive + "]Translation model loaded successfully[-]")
		})
	}()
}

func initTranscriber() {
	t := transcribe.New()
	sessionManager.Transcriber = t

	go func() {
		var lastPct int
		err := t.Init("", func(downloaded, total int64) {
			if total > 0 {
				pct := int(float64(downloaded) / float64(total) * 100)
				if pct != lastPct {
					lastPct = pct
					msg := fmt.Sprintf(" [::d]Downloading transcription model: %d%%[::-]", pct)
					go app.QueueUpdateDraw(func() {
						topBar.SetText(msg)
					})
				}
			}
		})
		if err != nil {
			go app.QueueUpdateDraw(func() {
				topBar.SetText("[::b] WhatsCLI " + VERSION + "[-::-]")
				PrintText("[" + config.Config.Colors.Negative + "]Transcription model failed to load: " + err.Error() + "[-]")
			})
			return
		}
		go app.QueueUpdateDraw(func() {
			topBar.SetText("[::b] WhatsCLI " + VERSION + "[-::-]")
			PrintText("[" + config.Config.Colors.Positive + "]Transcription model loaded successfully[-]")
		})
		sessionManager.TranscribeCurrentChat()
	}()
}

func handleTranslateCommand(arg string) {
	arg = strings.TrimSpace(arg)
	switch arg {
	case "on":
		if sessionManager.Translator == nil {
			initTranslator()
			PrintText("[" + config.Config.Colors.Positive + "]Translation enabled (loading model...)[-]")
		} else if !sessionManager.Translator.IsReady() {
			PrintText("Translation model is still loading...")
		} else {
			PrintText("Translation is already enabled")
		}
	case "off":
		if sessionManager.Translator != nil {
			sessionManager.Translator.Close()
			sessionManager.Translator = nil
			PrintText("[" + config.Config.Colors.Positive + "]Translation disabled[-]")
		} else {
			PrintText("Translation is already disabled")
		}
	default:
		if sessionManager.Translator != nil && sessionManager.Translator.IsReady() {
			sessionManager.Translator.Close()
			sessionManager.Translator = nil
			PrintText("[" + config.Config.Colors.Positive + "]Translation disabled[-]")
		} else if sessionManager.Translator != nil {
			PrintText("Translation model is still loading...")
		} else {
			initTranslator()
			PrintText("[" + config.Config.Colors.Positive + "]Translation enabled (loading model...)[-]")
		}
	}
}
