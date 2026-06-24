//! Helpers for rendering message timestamps and day-boundary dividers,
//! Slack-style.
//!
//! Design notes:
//! - "Today"/"Yesterday" are computed against the **local** calendar date, so
//!   the day boundary is local midnight (matches the phone's behavior).
//! - Weekday names are used for messages within the last 6 days so you can
//!   say "it was Monday" without doing date arithmetic in your head.
//! - Same-year messages collapse to "Wkday, Mon D"; older ones include year.
//! - Times are 24h `HH:MM`. No seconds.
//!
//! All functions are pure over an injected "now" so they're trivial to unit
//! test; the renderer captures `now` once per draw.

use time::{Duration, OffsetDateTime, UtcOffset, Weekday};

/// Returns the local date ordinal key used to detect day boundaries.
/// Two messages produce the same key if and only if they fall on the same
/// local calendar day.
pub fn local_day_key(timestamp: u64, offset: UtcOffset) -> (i32, u16) {
    let dt = OffsetDateTime::from_unix_timestamp(timestamp as i64)
        .unwrap_or(OffsetDateTime::UNIX_EPOCH)
        .to_offset(offset);
    (dt.year(), dt.ordinal())
}

/// Format a human-friendly label for a message's date, relative to `now`.
///
/// Ordering of rules (first match wins):
///   1. Same local day as now                       -> "Today"
///   2. Previous local day                          -> "Yesterday"
///   3. Within the previous 6 days                  -> "Monday", "Tuesday", ...
///   4. Same calendar year                          -> "Fri, Mar 28"
///   5. Older                                       -> "Fri, Mar 28 2024"
pub fn format_day_label(timestamp: u64, now: OffsetDateTime, offset: UtcOffset) -> String {
    let msg = OffsetDateTime::from_unix_timestamp(timestamp as i64)
        .unwrap_or(OffsetDateTime::UNIX_EPOCH)
        .to_offset(offset);
    let now = now.to_offset(offset);

    let msg_date = msg.date();
    let now_date = now.date();

    if msg_date == now_date {
        return "Today".to_string();
    }
    if msg_date == now_date - Duration::days(1) {
        return "Yesterday".to_string();
    }

    let age = now_date - msg_date;
    if age > Duration::ZERO && age < Duration::days(7) {
        return weekday_name(msg.weekday()).to_string();
    }

    let wk = weekday_short(msg.weekday());
    let mo = month_short(msg.month());
    let day = msg.day();
    if msg_date.year() == now_date.year() {
        format!("{}, {} {}", wk, mo, day)
    } else {
        format!("{}, {} {} {}", wk, mo, day, msg_date.year())
    }
}

/// Format a message's clock time in local 24h HH:MM.
pub fn format_clock(timestamp: u64, offset: UtcOffset) -> String {
    let dt = OffsetDateTime::from_unix_timestamp(timestamp as i64)
        .unwrap_or(OffsetDateTime::UNIX_EPOCH)
        .to_offset(offset);
    format!("{:02}:{:02}", dt.hour(), dt.minute())
}

/// The local UTC offset, or `UTC` as a safe fallback if the OS won't tell us.
///
/// On some platforms (e.g. multithreaded Linux without the `TZ` env var set)
/// `UtcOffset::current_local_offset()` can fail. Falling back to UTC in that
/// case only affects day-boundary labelling, never the ordering of messages.
pub fn current_local_offset() -> UtcOffset {
    UtcOffset::current_local_offset().unwrap_or(UtcOffset::UTC)
}

fn weekday_name(w: Weekday) -> &'static str {
    match w {
        Weekday::Monday => "Monday",
        Weekday::Tuesday => "Tuesday",
        Weekday::Wednesday => "Wednesday",
        Weekday::Thursday => "Thursday",
        Weekday::Friday => "Friday",
        Weekday::Saturday => "Saturday",
        Weekday::Sunday => "Sunday",
    }
}

fn weekday_short(w: Weekday) -> &'static str {
    match w {
        Weekday::Monday => "Mon",
        Weekday::Tuesday => "Tue",
        Weekday::Wednesday => "Wed",
        Weekday::Thursday => "Thu",
        Weekday::Friday => "Fri",
        Weekday::Saturday => "Sat",
        Weekday::Sunday => "Sun",
    }
}

fn month_short(m: time::Month) -> &'static str {
    use time::Month::*;
    match m {
        January => "Jan",
        February => "Feb",
        March => "Mar",
        April => "Apr",
        May => "May",
        June => "Jun",
        July => "Jul",
        August => "Aug",
        September => "Sep",
        October => "Oct",
        November => "Nov",
        December => "Dec",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use time::macros::datetime;

    /// Fixed reference moment for stable tests: 2026-04-20 14:30 UTC, a Monday.
    fn now_utc() -> OffsetDateTime {
        datetime!(2026-04-20 14:30:00 UTC)
    }

    fn ts(dt: OffsetDateTime) -> u64 {
        dt.unix_timestamp() as u64
    }

    #[test]
    fn today_label() {
        let t = ts(datetime!(2026-04-20 09:15:00 UTC));
        assert_eq!(format_day_label(t, now_utc(), UtcOffset::UTC), "Today");
    }

    #[test]
    fn yesterday_label() {
        let t = ts(datetime!(2026-04-19 23:59:00 UTC));
        assert_eq!(format_day_label(t, now_utc(), UtcOffset::UTC), "Yesterday");
    }

    #[test]
    fn weekday_within_last_6_days() {
        // Two days before a Monday is Saturday.
        let t = ts(datetime!(2026-04-18 10:00:00 UTC));
        assert_eq!(format_day_label(t, now_utc(), UtcOffset::UTC), "Saturday");
    }

    #[test]
    fn older_same_year() {
        let t = ts(datetime!(2026-03-28 10:00:00 UTC));
        assert_eq!(format_day_label(t, now_utc(), UtcOffset::UTC), "Sat, Mar 28");
    }

    #[test]
    fn older_with_year() {
        let t = ts(datetime!(2024-03-28 10:00:00 UTC));
        assert_eq!(
            format_day_label(t, now_utc(), UtcOffset::UTC),
            "Thu, Mar 28 2024"
        );
    }

    #[test]
    fn clock_is_24h() {
        let t = ts(datetime!(2026-04-20 23:05:00 UTC));
        assert_eq!(format_clock(t, UtcOffset::UTC), "23:05");
    }

    #[test]
    fn clock_respects_offset() {
        let t = ts(datetime!(2026-04-20 23:05:00 UTC));
        // Costa Rica is UTC-6, so 23:05 UTC reads as 17:05 local.
        let cr = UtcOffset::from_hms(-6, 0, 0).unwrap();
        assert_eq!(format_clock(t, cr), "17:05");
    }

    #[test]
    fn day_key_groups_same_local_day() {
        // Two messages 10 hours apart on the same UTC day share a key.
        let a = ts(datetime!(2026-04-20 01:00:00 UTC));
        let b = ts(datetime!(2026-04-20 23:00:00 UTC));
        assert_eq!(
            local_day_key(a, UtcOffset::UTC),
            local_day_key(b, UtcOffset::UTC)
        );
    }

    #[test]
    fn day_key_changes_across_local_midnight() {
        let a = ts(datetime!(2026-04-20 23:30:00 UTC));
        let b = ts(datetime!(2026-04-21 00:30:00 UTC));
        assert_ne!(
            local_day_key(a, UtcOffset::UTC),
            local_day_key(b, UtcOffset::UTC)
        );
    }
}
