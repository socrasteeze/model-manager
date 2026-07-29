// Package timestamp provides the one timestamp format this project stores.
//
// It exists because time.RFC3339Nano is the wrong choice for stored values: it
// trims trailing zeros from the fractional seconds, so ".1Z" and ".12Z" are both
// valid encodings of different instants that do NOT compare correctly as
// strings -- '1' == '1', then 'Z' (0x5A) > '2' (0x32), which makes the earlier
// instant sort later.
//
// That matters because timestamps here are compared as strings in places that
// would each be silently wrong: SQL ORDER BY on last_verified, the SQL expiry
// check on the negative origin cache, and the LRU ordering that decides which
// staged model gets evicted from an SSD tier.
//
// Fixing it at the source beats remembering to parse at every comparison. The
// layout below is fixed-width, still valid RFC 3339, and sorts lexicographically
// in chronological order.
package timestamp

import "time"

// Layout is fixed-width RFC 3339 with nine fractional digits.
const Layout = "2006-01-02T15:04:05.000000000Z07:00"

// Now returns the current UTC time in the storage format.
func Now() string { return time.Now().UTC().Format(Layout) }

// Format renders a time in the storage format.
func Format(t time.Time) string { return t.UTC().Format(Layout) }

// Parse reads a stored timestamp, accepting the older variable-width encoding so
// a database written before this change still loads.
func Parse(s string) (time.Time, error) {
	if t, err := time.Parse(Layout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
