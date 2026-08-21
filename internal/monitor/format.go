package monitor

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Bucket is one column of a distribution chart: an inclusive upper bound and the
// label that owns everything up to it.
type Bucket struct {
	Label string
	Max   int64
	// NoMax marks a bucket that no numeric comparison can reach — the "No expiry"
	// column, which only the TTL classifier assigns.
	NoMax bool
}

// The bucket label sets are fixed constants rather than anything derived from the
// data. A chart whose buckets moved between two refreshes would be impossible to
// read a trend from, so the axis stays put and only the counts change.
var (
	// TTLBuckets are seconds, inclusive upper bound.
	TTLBuckets = []Bucket{
		{Label: "No expiry", NoMax: true},
		{Label: "≤ 1 min", Max: 60},
		{Label: "1 – 5 min", Max: 300},
		{Label: "5 – 60 min", Max: 3600},
		{Label: "1 – 24 hrs", Max: 86400},
		{Label: "> 24 hrs", Max: math.MaxInt64},
	}

	// SizeBuckets are bytes of MEMORY USAGE, inclusive upper bound.
	SizeBuckets = []Bucket{
		{Label: "≤ 128 B", Max: 128},
		{Label: "≤ 1 KB", Max: 1024},
		{Label: "≤ 8 KB", Max: 8192},
		{Label: "≤ 64 KB", Max: 65536},
		{Label: "≤ 512 KB", Max: 524288},
		{Label: "≤ 4 MB", Max: 4194304},
		{Label: "> 4 MB", Max: math.MaxInt64},
	}

	// IdleBuckets are seconds of OBJECT IDLETIME — how cold a key is.
	IdleBuckets = []Bucket{
		{Label: "Hot (< 1 min)", Max: 60},
		{Label: "1 – 10 min", Max: 600},
		{Label: "10 – 60 min", Max: 3600},
		{Label: "1 – 24 hrs", Max: 86400},
		{Label: "1 – 7 days", Max: 604800},
		{Label: "Cold (> 7 days)", Max: math.MaxInt64},
	}
)

// noPrefix is the label for a key that is not namespaced at all. It is not a
// namespace: no glob selects exactly this group, which is why the dropdown and the
// "browse these keys" links leave its pattern null.
const noPrefix = "(no prefix)"

// CountRow is one bar: a label and its count, with an optional human-readable
// rendering when the count is a byte figure.
type CountRow struct {
	Label string  `json:"label"`
	Count int64   `json:"count"`
	Human *string `json:"human,omitempty"`
	Keys  *int64  `json:"keys,omitempty"`
}

// bucketLabel classifies a value into the first bucket whose upper bound it fits.
func bucketLabel(value int64, buckets []Bucket) string {
	for _, bucket := range buckets {
		if !bucket.NoMax && value <= bucket.Max {
			return bucket.Label
		}
	}

	if len(buckets) == 0 {
		return ""
	}

	return buckets[len(buckets)-1].Label
}

// ttlBucketLabel classifies a TTL, where anything negative means the key has no
// expiry at all rather than a very small one.
func ttlBucketLabel(ttl int64) string {
	if ttl < 0 {
		return TTLBuckets[0].Label
	}

	return bucketLabel(ttl, TTLBuckets)
}

// bucketRows renders a distribution in bucket order, keeping empty buckets so the
// shape of the chart stays comparable between two refreshes.
func bucketRows(counts map[string]int64, buckets []Bucket) []CountRow {
	rows := make([]CountRow, 0, len(buckets))

	for _, bucket := range buckets {
		rows = append(rows, CountRow{Label: bucket.Label, Count: counts[bucket.Label]})
	}

	return rows
}

// countRows renders an unordered tally largest first. Ties break on the label so
// two refreshes of an unchanged keyspace produce an identical payload.
func countRows(counts map[string]int64) []CountRow {
	rows := make([]CountRow, 0, len(counts))

	for label, count := range counts {
		rows = append(rows, CountRow{Label: label, Count: count})
	}

	sortCountRows(rows)

	return rows
}

// topRows keeps the largest n and folds the remainder into one "Other" bar, rather
// than dropping it — a chart that silently omits half the keyspace is worse than
// one honest catch-all column.
func topRows(counts map[string]int64, n int) []CountRow {
	rows := countRows(counts)

	if n < 1 || len(rows) <= n {
		return rows
	}

	var other int64
	for _, row := range rows[n:] {
		other += row.Count
	}

	kept := rows[:n:n]

	if other > 0 {
		kept = append(kept, CountRow{Label: "Other", Count: other})
	}

	return kept
}

func sortCountRows(rows []CountRow) {
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].Count != rows[b].Count {
			return rows[a].Count > rows[b].Count
		}

		return rows[a].Label < rows[b].Label
	})
}

// namespaceOf returns a key's leading segment including its separator, or false
// when the key is not namespaced.
//
// Redis has no namespace concept; this is the convention keys actually follow.
// Kept apart from prefixOf because the dropdown has to tell "no namespace" from a
// namespace literally called "(no prefix)", and because the absence of one means
// there is no glob that selects exactly that group.
func namespaceOf(key string) (string, bool) {
	for _, separator := range []string{":", "|", "/"} {
		if position := strings.Index(key, separator); position > 0 {
			return key[:position] + separator, true
		}
	}

	return "", false
}

// prefixOf is namespaceOf with a label for the unnamespaced case, for charts that
// need every key to land somewhere.
func prefixOf(key string) string {
	if namespace, ok := namespaceOf(key); ok {
		return namespace
	}

	return noPrefix
}

// globEscape escapes the metacharacters Redis honours in a MATCH pattern, so a
// literal key fragment matches only itself — a prefix containing '*' or '[' would
// otherwise select far more than its own group.
func globEscape(literal string) string {
	var out strings.Builder
	out.Grow(len(literal) + 8)

	for _, char := range literal {
		switch char {
		case '\\', '*', '?', '[', ']', '^':
			out.WriteByte('\\')
		}

		out.WriteRune(char)
	}

	return out.String()
}

// namespacePattern is the ready-made MATCH glob that selects one namespace.
func namespacePattern(namespace string) string {
	return globEscape(namespace) + "*"
}

// ttlLabel renders a TTL for a table cell. -1 is "no expiry" and -2 is a key that
// has gone since it was scanned.
func ttlLabel(ttl int64) string {
	switch {
	case ttl == -1:
		return "no expiry"
	case ttl == -2:
		return "expired"
	case ttl < 0:
		return "no expiry"
	case ttl < 60:
		return strconv.FormatInt(ttl, 10) + "s"
	case ttl < 3600:
		return fmt.Sprintf("%dm %ds", ttl/60, ttl%60)
	case ttl < 86400:
		return fmt.Sprintf("%dh %dm", ttl/3600, (ttl%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", ttl/86400, (ttl%86400)/3600)
	}
}

// humanBytes renders a byte count at one decimal place, dropping the decimal once
// the number is big enough that it adds nothing.
func humanBytes(bytes int64) string {
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10) + " B"
	}

	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes) / 1024
	unit := 0

	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}

	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[unit])
	}

	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + units[unit]
}

// round1 rounds to one decimal place, for the percentages the payload reports.
func round1(value float64) float64 {
	return math.Round(value*10) / 10
}

// percent is a share of a total, or nil when there is nothing to divide by — an
// unknown rate must not render as 0%.
func percent(part, total int64) *float64 {
	if total <= 0 {
		return nil
	}

	value := round1(float64(part) / float64(total) * 100)

	return &value
}

func humanPtr(bytes int64) *string {
	human := humanBytes(bytes)

	return &human
}

func int64Ptr(value int64) *int64 {
	return &value
}

func atLeastInt(value, floor int) int {
	if value < floor {
		return floor
	}

	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}

	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}

	return b
}
