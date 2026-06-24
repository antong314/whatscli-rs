//! Word-aware line wrapping for chat text.
//!
//! The previous implementation sliced strings by `chars[..n]`, which had two
//! problems:
//!
//!   1. It cut words in half at the column edge ("when" -> "wh" + "en"), which
//!      is hard to read in long messages.
//!   2. It treated every Unicode codepoint as one column wide. WhatsApp chats
//!      are full of emoji and the occasional CJK character, both of which take
//!      two terminal columns; counting them as one walked the wrap boundary
//!      past the right edge of the viewport.
//!
//! This module fixes both. Wrapping is greedy ("first-fit"), which is what
//! users expect from chat clients - it doesn't try to balance ragged-right
//! lines like a typesetting algorithm would. A token longer than the available
//! width is hard-broken at the column edge so we never overflow the viewport.

use unicode_width::UnicodeWidthChar;

/// Wrap `text` to lines that each fit within `width` terminal columns.
///
/// Splits on ASCII whitespace and treats each non-whitespace run as an
/// indivisible "word". Single-space separators are preserved between words on
/// the same line; we collapse runs of whitespace to a single space because
/// that's what a chat client should do (and matches what a recipient typed
/// with a soft-wrapping keyboard).
///
/// A word longer than `width` (e.g. a long URL or a hash) is hard-broken on
/// column boundaries: we never produce a line wider than `width`.
pub fn wrap_words(text: &str, width: usize) -> Vec<String> {
    if width == 0 {
        return Vec::new();
    }
    let mut lines: Vec<String> = Vec::new();
    let mut current = String::new();
    let mut current_width: usize = 0;

    for word in text.split_whitespace() {
        let word_w = display_width(word);

        if current.is_empty() {
            // First word on the line: place it (or break it if it's too wide).
            if word_w <= width {
                current.push_str(word);
                current_width = word_w;
            } else {
                push_hard_broken_word(&mut lines, &mut current, &mut current_width, word, width);
            }
            continue;
        }

        // Try to append " <word>" to the current line.
        if current_width + 1 + word_w <= width {
            current.push(' ');
            current.push_str(word);
            current_width += 1 + word_w;
        } else if word_w <= width {
            // Word fits on its own line.
            lines.push(std::mem::take(&mut current));
            current.push_str(word);
            current_width = word_w;
        } else {
            // Word is wider than `width`. Flush the current line, then split
            // the giant word across as many lines as it takes.
            lines.push(std::mem::take(&mut current));
            current_width = 0;
            push_hard_broken_word(&mut lines, &mut current, &mut current_width, word, width);
        }
    }

    if !current.is_empty() {
        lines.push(current);
    }

    if lines.is_empty() {
        // Preserve "I asked to wrap something, give me at least one line back"
        // so callers can rely on `wrap_words(...).len() >= 1` when text is
        // non-empty, but only when the text actually had visible content. For
        // pure whitespace, returning empty is the right answer.
        if !text.is_empty() && text.chars().any(|c| !c.is_whitespace()) {
            lines.push(String::new());
        } else if !text.is_empty() {
            // Pure-whitespace input: emit a single blank so we don't lose the
            // line entirely.
            lines.push(String::new());
        }
    }

    lines
}

/// Hard-break `word` (which is wider than `width`) into chunks that each fit
/// within `width` columns and push them as new lines, leaving `current` set
/// to the trailing partial chunk (or empty, if the word ended exactly on a
/// boundary).
fn push_hard_broken_word(
    lines: &mut Vec<String>,
    current: &mut String,
    current_width: &mut usize,
    word: &str,
    width: usize,
) {
    debug_assert!(current.is_empty(), "caller must flush before hard-breaking");
    debug_assert!(width > 0);

    for ch in word.chars() {
        let cw = char_display_width(ch);
        if *current_width + cw > width {
            lines.push(std::mem::take(current));
            *current_width = 0;
        }
        current.push(ch);
        *current_width += cw;
    }
}

/// Display width in terminal columns. Treats unprintable / control chars as
/// zero-width and unknown chars as 1 (matches `unicode-width`'s behaviour for
/// `width()` returning `None`).
pub fn display_width(s: &str) -> usize {
    s.chars().map(char_display_width).sum()
}

fn char_display_width(c: char) -> usize {
    UnicodeWidthChar::width(c).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wraps_at_word_boundary_not_mid_word() {
        let text = "the stress level can rise quickly when two or three kids are shouting";
        let wrapped = wrap_words(text, 30);
        for line in &wrapped {
            assert!(
                display_width(line) <= 30,
                "line wider than 30: {:?} ({} cols)",
                line,
                display_width(line)
            );
            // No line should end in a partial word: every line must either be
            // the last one, or end at a whitespace boundary in the original.
            // We can't easily check that here since wrap_words drops separator
            // whitespace, but we CAN check that no line starts with a partial
            // word-tail like "en" by joining with spaces and verifying we
            // recover the original token sequence.
        }
        let recovered = wrapped.join(" ");
        assert_eq!(recovered, text);
    }

    #[test]
    fn never_emits_a_line_wider_than_target() {
        let text = "alpha bravo charlie delta echo foxtrot golf hotel india juliet";
        for w in [5_usize, 10, 13, 20, 40] {
            let wrapped = wrap_words(text, w);
            for line in &wrapped {
                assert!(
                    display_width(line) <= w,
                    "width={} produced overlong line {:?}",
                    w,
                    line
                );
            }
        }
    }

    #[test]
    fn hard_breaks_word_longer_than_width() {
        let text = "supercalifragilisticexpialidocious";
        let wrapped = wrap_words(text, 10);
        // Every line is at most 10 columns; concatenated they reproduce the word.
        for line in &wrapped {
            assert!(display_width(line) <= 10);
        }
        let joined: String = wrapped.join("");
        assert_eq!(joined, text);
    }

    #[test]
    fn splits_long_word_after_short_word_starts_word_on_its_own_line() {
        // "a" fits, then a giant word should move to a new line and break.
        let text = "a supercalifragilisticexpialidocious";
        let wrapped = wrap_words(text, 10);
        assert_eq!(wrapped[0], "a");
        // The remainder is the big word, hard-broken into <=10-column chunks.
        let rest: String = wrapped[1..].concat();
        assert_eq!(rest, "supercalifragilisticexpialidocious");
    }

    #[test]
    fn handles_emoji_as_double_width() {
        // Each "💎" is 2 columns wide. With width=6 we can fit at most 3.
        let wrapped = wrap_words("💎💎💎💎💎", 6);
        for line in &wrapped {
            assert!(
                display_width(line) <= 6,
                "{:?} too wide ({} cols)",
                line,
                display_width(line)
            );
        }
    }

    #[test]
    fn collapses_internal_whitespace_runs_to_single_space() {
        // Multiple spaces between words become single spaces. This matches
        // what a chat user typed-and-sent; we're not reflowing source code.
        let wrapped = wrap_words("hello     world", 80);
        assert_eq!(wrapped, vec!["hello world"]);
    }

    #[test]
    fn empty_input_returns_empty() {
        assert!(wrap_words("", 80).is_empty());
    }

    #[test]
    fn whitespace_only_input_returns_blank_line() {
        // Preserve the line break so callers don't silently drop blank lines.
        assert_eq!(wrap_words("   ", 80), vec![String::new()]);
    }

    #[test]
    fn zero_width_returns_empty() {
        assert!(wrap_words("hello", 0).is_empty());
    }
}
