package monitor

import (
	"math"
	"testing"
)

func TestTTLLabel(t *testing.T) {
	cases := []struct {
		ttl  int64
		want string
	}{
		{-1, "no expiry"},
		{-2, "expired"},
		{0, "0s"},
		{45, "45s"},
		{59, "59s"},
		{60, "1m 0s"},
		{125, "2m 5s"},
		{3599, "59m 59s"},
		{3600, "1h 0m"},
		{7325, "2h 2m"},
		{86399, "23h 59m"},
		{86400, "1d 0h"},
		{90061, "1d 1h"},
	}

	for _, testCase := range cases {
		if got := ttlLabel(testCase.ttl); got != testCase.want {
			t.Errorf("ttlLabel(%d) = %q, want %q", testCase.ttl, got, testCase.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1 KB"},
		{1536, "1.5 KB"},
		// Past 100 the decimal adds nothing, so it is dropped.
		{153600, "150 KB"},
		{1048576, "1 MB"},
		{1572864, "1.5 MB"},
		{1073741824, "1 GB"},
		{1099511627776, "1 TB"},
		// Beyond the last unit the number keeps growing rather than inventing a PB.
		{2199023255552, "2 TB"},
	}

	for _, testCase := range cases {
		if got := humanBytes(testCase.bytes); got != testCase.want {
			t.Errorf("humanBytes(%d) = %q, want %q", testCase.bytes, got, testCase.want)
		}
	}
}

func TestTTLBucketLabel(t *testing.T) {
	cases := []struct {
		ttl  int64
		want string
	}{
		// Anything negative means no expiry at all, not a very small one.
		{-1, "No expiry"},
		{-2, "No expiry"},
		{0, "≤ 1 min"},
		{60, "≤ 1 min"},
		{61, "1 – 5 min"},
		{300, "1 – 5 min"},
		{301, "5 – 60 min"},
		{3600, "5 – 60 min"},
		{3601, "1 – 24 hrs"},
		{86400, "1 – 24 hrs"},
		{86401, "> 24 hrs"},
		{math.MaxInt64, "> 24 hrs"},
	}

	for _, testCase := range cases {
		if got := ttlBucketLabel(testCase.ttl); got != testCase.want {
			t.Errorf("ttlBucketLabel(%d) = %q, want %q", testCase.ttl, got, testCase.want)
		}
	}
}

func TestBucketLabelBoundaries(t *testing.T) {
	cases := []struct {
		value   int64
		buckets []Bucket
		want    string
	}{
		{128, SizeBuckets, "≤ 128 B"},
		{129, SizeBuckets, "≤ 1 KB"},
		{4194304, SizeBuckets, "≤ 4 MB"},
		{4194305, SizeBuckets, "> 4 MB"},
		{0, IdleBuckets, "Hot (< 1 min)"},
		{604800, IdleBuckets, "1 – 7 days"},
		{604801, IdleBuckets, "Cold (> 7 days)"},
	}

	for _, testCase := range cases {
		if got := bucketLabel(testCase.value, testCase.buckets); got != testCase.want {
			t.Errorf("bucketLabel(%d) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

func TestBucketRowsKeepsEmptyBuckets(t *testing.T) {
	rows := bucketRows(map[string]int64{"≤ 1 KB": 3}, SizeBuckets)

	if len(rows) != len(SizeBuckets) {
		t.Fatalf("got %d rows, want %d — empty buckets must be kept so the chart shape stays comparable", len(rows), len(SizeBuckets))
	}

	if rows[0].Label != "≤ 128 B" || rows[0].Count != 0 {
		t.Errorf("first row = %+v, want the empty ≤ 128 B bucket", rows[0])
	}

	if rows[1].Count != 3 {
		t.Errorf("≤ 1 KB count = %d, want 3", rows[1].Count)
	}
}

func TestNamespaceOf(t *testing.T) {
	cases := []struct {
		key       string
		want      string
		namespace bool
	}{
		{"app_cache:config", "app_cache:", true},
		{"queue|default", "queue|", true},
		{"path/to/thing", "path/", true},
		// Separators are tried in priority order — ':' then '|' then '/' — not by
		// position. So a ':' later in the key still wins over an earlier '|'.
		{"a:b|c/d", "a:", true},
		{"queue|a:b", "queue|a:", true},
		{"queue|default/x", "queue|", true},
		// No separator at all means no namespace — and no glob that selects it.
		{"plainkey", "", false},
		// A leading separator is not a namespace boundary: the name would be empty.
		{":leading", "", false},
	}

	for _, testCase := range cases {
		got, ok := namespaceOf(testCase.key)

		if ok != testCase.namespace || got != testCase.want {
			t.Errorf("namespaceOf(%q) = (%q, %v), want (%q, %v)",
				testCase.key, got, ok, testCase.want, testCase.namespace)
		}
	}
}

func TestPrefixOfLabelsUnnamespacedKeys(t *testing.T) {
	if got := prefixOf("plainkey"); got != noPrefix {
		t.Errorf("prefixOf(plainkey) = %q, want %q", got, noPrefix)
	}

	if got := prefixOf("cache:thing"); got != "cache:" {
		t.Errorf("prefixOf(cache:thing) = %q, want %q", got, "cache:")
	}
}

func TestGlobEscape(t *testing.T) {
	cases := []struct {
		literal string
		want    string
	}{
		{"cache:", "cache:"},
		{"we*rd:", `we\*rd:`},
		{"br[a]cket:", `br\[a\]cket:`},
		{"ques?ion:", `ques\?ion:`},
		{`back\slash:`, `back\\slash:`},
		{"ca^ret:", `ca\^ret:`},
	}

	for _, testCase := range cases {
		if got := globEscape(testCase.literal); got != testCase.want {
			t.Errorf("globEscape(%q) = %q, want %q", testCase.literal, got, testCase.want)
		}
	}
}

func TestNamespacePatternEscapesMetacharacters(t *testing.T) {
	// A prefix containing '*' must still select only itself.
	if got := namespacePattern("we*rd:"); got != `we\*rd:*` {
		t.Errorf("namespacePattern(we*rd:) = %q, want %q", got, `we\*rd:*`)
	}
}

func TestTopRowsFoldsRemainderIntoOther(t *testing.T) {
	rows := topRows(map[string]int64{
		"a:": 10,
		"b:": 8,
		"c:": 5,
		"d:": 2,
		"e:": 1,
	}, 3)

	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 3 + Other", len(rows))
	}

	if rows[0].Label != "a:" || rows[0].Count != 10 {
		t.Errorf("first row = %+v, want the largest namespace first", rows[0])
	}

	last := rows[len(rows)-1]

	if last.Label != "Other" || last.Count != 3 {
		t.Errorf("last row = %+v, want Other with the folded remainder (2+1)", last)
	}
}

func TestTopRowsWithoutRemainderHasNoOtherBar(t *testing.T) {
	rows := topRows(map[string]int64{"a:": 2, "b:": 1}, 5)

	for _, row := range rows {
		if row.Label == "Other" {
			t.Fatalf("unexpected Other bar when nothing was folded: %+v", rows)
		}
	}
}

func TestCountRowsIsDeterministic(t *testing.T) {
	// Equal counts must break on the label, or two refreshes of an unchanged
	// keyspace produce different payloads and the chart jitters.
	counts := map[string]int64{"zebra": 4, "apple": 4, "mango": 9}

	for attempt := 0; attempt < 20; attempt++ {
		rows := countRows(counts)

		if rows[0].Label != "mango" || rows[1].Label != "apple" || rows[2].Label != "zebra" {
			t.Fatalf("unstable ordering: %+v", rows)
		}
	}
}

func TestPercentIsNilWhenThereIsNothingToDivideBy(t *testing.T) {
	if got := percent(5, 0); got != nil {
		t.Errorf("percent(5, 0) = %v, want nil — an unknown rate must not render as 0%%", *got)
	}

	got := percent(1, 3)

	if got == nil || *got != 33.3 {
		t.Errorf("percent(1, 3) = %v, want 33.3", got)
	}
}
