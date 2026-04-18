#!/usr/bin/env bash
# E2E test harness for whatscli-tui using tmux
# Usage: ./tests/tmux_e2e.sh [test_name]
# Requires: tmux, whatscli-server running

set -euo pipefail

SESSION="whatscli-e2e"
SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
TUI_BIN="$SCRIPT_DIR/target/release/whatscli-tui"
STDERR_LOG="/tmp/tui-e2e-stderr.log"
COLS=120
ROWS=40
PASS=0
FAIL=0

cleanup() {
    tmux kill-session -t "$SESSION" 2>/dev/null || true
}
trap cleanup EXIT

capture() {
    tmux capture-pane -t "$SESSION" -p 2>/dev/null
}

title_line() {
    capture | head -1
}

send_key() {
    tmux send-keys -t "$SESSION" "$1"
}

wait_for_tui() {
    local max_wait=${1:-10}
    local i=0
    while [ $i -lt $max_wait ]; do
        local title
        title=$(title_line)
        if echo "$title" | grep -q "●"; then
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    echo "TIMEOUT: TUI did not start within ${max_wait}s"
    return 1
}

wait_for_chat() {
    local expected="$1"
    local max_wait=${2:-5}
    local i=0
    while [ $i -lt $max_wait ]; do
        local title
        title=$(title_line)
        if echo "$title" | grep -qF "$expected"; then
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    return 1
}

assert_title_contains() {
    local expected="$1"
    local label="${2:-$expected}"
    local title
    title=$(title_line)
    if echo "$title" | grep -qF "$expected"; then
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $label (expected '$expected' in title, got '$title')"
        FAIL=$((FAIL + 1))
    fi
}

assert_screen_contains() {
    local pattern="$1"
    local label="${2:-contains $pattern}"
    local screen
    screen=$(capture)
    if echo "$screen" | grep -qF "$pattern"; then
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $label (pattern '$pattern' not found in screen)"
        FAIL=$((FAIL + 1))
    fi
}

assert_screen_not_contains() {
    local pattern="$1"
    local label="${2:-not contains $pattern}"
    local screen
    screen=$(capture)
    if echo "$screen" | grep -qF "$pattern"; then
        echo "  FAIL: $label (pattern '$pattern' unexpectedly found)"
        FAIL=$((FAIL + 1))
    else
        echo "  PASS: $label"
        PASS=$((PASS + 1))
    fi
}

start_tui() {
    cleanup
    tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS"
    sleep 0.5
    send_key "$TUI_BIN 2>$STDERR_LOG"
    send_key Enter
    wait_for_tui 10
}

# --- Tests ---

test_startup() {
    echo "TEST: startup"
    start_tui
    assert_title_contains "WhatsCLI" "title shows WhatsCLI"
    assert_screen_contains "Chats" "chat list visible"
    assert_screen_contains "Keyboard shortcuts" "help text shown"
    assert_screen_contains "Archived" "archived folder visible"
}

test_navigate_down() {
    echo "TEST: navigate down selects chat"
    start_tui
    send_key Down
    sleep 2
    assert_title_contains "Gilberth Madera" "first chat selected"
    assert_screen_not_contains "Keyboard shortcuts" "help text replaced by messages"
}

test_navigate_up_down() {
    echo "TEST: navigate up and down between chats"
    start_tui
    send_key Down
    sleep 2
    assert_title_contains "Gilberth Madera" "at Gilberth Madera"

    send_key Down
    sleep 2
    assert_title_contains "Elvi Gorshkov" "moved to Elvi"

    send_key Up
    sleep 2
    assert_title_contains "Gilberth Madera" "back to Gilberth Madera"
}

test_translation() {
    echo "TEST: translations appear for non-English chats"
    start_tui
    # Navigate to "Gilberth" (index 7, the Spanish chat with many messages)
    local i
    for i in 1 2 3 4 5 6; do
        send_key Down; sleep 0.3
    done
    sleep 2
    assert_title_contains "Gilberth" "at Gilberth chat"

    # Translations are async, wait for them
    i=0
    local found=0
    while [ $i -lt 15 ]; do
        sleep 2
        local screen
        screen=$(capture)
        if echo "$screen" | grep -qE "\(EN\):"; then
            found=1
            break
        fi
        i=$((i + 1))
    done
    if [ "$found" -eq 1 ]; then
        echo "  PASS: translation markers (EN) found (after ~$((i * 2))s)"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: no (EN) translation markers found after 30s"
        FAIL=$((FAIL + 1))
    fi
}

test_english_no_translation() {
    echo "TEST: English chats should not be translated"
    start_tui
    # Navigate to Elvi Gorshkov (English chat)
    send_key Down; sleep 0.5
    send_key Down; sleep 2
    assert_title_contains "Elvi Gorshkov" "at Elvi"
    local screen
    screen=$(capture)
    local en_count
    en_count=$(echo "$screen" | grep -c "(EN):" || true)
    if [ "$en_count" -eq 0 ]; then
        echo "  PASS: no translations in English chat"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: found $en_count translation markers in English chat"
        FAIL=$((FAIL + 1))
    fi
}

test_image_placeholder() {
    echo "TEST: image messages show [IMAGE] placeholder"
    start_tui
    # Navigate to Finca Oma (has images)
    send_key Down; sleep 0.3
    send_key Down; sleep 0.3
    send_key Down; sleep 0.3
    send_key Down; sleep 3
    assert_title_contains "Finca Oma" "at Finca Oma"
    assert_screen_contains "[IMAGE]" "image placeholder visible"
    assert_screen_not_contains "[Loading image...]" "no stuck loading indicators"
}

test_ctrl_q_quits() {
    echo "TEST: Ctrl+Q quits the application"
    start_tui
    send_key C-q
    sleep 2
    local screen
    screen=$(capture)
    if echo "$screen" | grep -q "●"; then
        echo "  FAIL: TUI still running after Ctrl+Q"
        FAIL=$((FAIL + 1))
    else
        echo "  PASS: TUI exited"
        PASS=$((PASS + 1))
    fi
}

test_scroll_messages() {
    echo "TEST: PageUp/PageDown scrolls messages"
    start_tui
    # Navigate to a chat with many messages (Finca Oma)
    send_key Down; sleep 0.3
    send_key Down; sleep 0.3
    send_key Down; sleep 0.3
    send_key Down; sleep 3
    assert_title_contains "Finca Oma" "at Finca Oma for scroll test"
    local before
    before=$(capture | md5)
    send_key PageUp; sleep 2
    local after
    after=$(capture | md5)
    if [ "$before" != "$after" ]; then
        echo "  PASS: screen changed after PageUp"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: screen unchanged after PageUp (md5 before=$before after=$after)"
        FAIL=$((FAIL + 1))
    fi
}

# --- Runner ---

run_all() {
    test_startup
    test_navigate_down
    test_navigate_up_down
    test_translation
    test_english_no_translation
    test_image_placeholder
    test_ctrl_q_quits
    test_scroll_messages
}

if [ $# -gt 0 ]; then
    "test_$1"
else
    run_all
fi

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
