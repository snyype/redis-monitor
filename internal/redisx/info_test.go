package redisx

import "testing"

const sampleInfo = "# Server\r\n" +
	"redis_version:5.0.14.1\r\n" +
	"redis_mode:standalone\r\n" +
	"os:Windows\r\n" +
	"uptime_in_seconds:1213\r\n" +
	"\r\n" +
	"# Clients\r\n" +
	"connected_clients:3\r\n" +
	"blocked_clients:0\r\n" +
	"\r\n" +
	"# Memory\r\n" +
	"used_memory:729096\r\n" +
	"maxmemory:0\r\n" +
	"maxmemory_policy:noeviction\r\n" +
	"mem_fragmentation_ratio:1.07\r\n" +
	"\r\n" +
	"# Persistence\r\n" +
	"aof_enabled:1\r\n" +
	"rdb_last_save_time:1787340247\r\n" +
	"\r\n" +
	"# Stats\r\n" +
	"keyspace_hits:240\r\n" +
	"keyspace_misses:1\r\n" +
	"\r\n" +
	"# Replication\r\n" +
	"role:master\r\n" +
	"\r\n" +
	"# Keyspace\r\n" +
	"db0:keys=35,expires=28,avg_ttl=42426211\r\n" +
	"db3:keys=7,expires=0,avg_ttl=0\r\n"

func TestParseInfoSectionsAndFlatBag(t *testing.T) {
	info := ParseInfo(sampleInfo)

	if got := info.Str("redis_version", ""); got != "5.0.14.1" {
		t.Errorf("redis_version = %q, want 5.0.14.1", got)
	}

	if got := info.Sections["Memory"]["used_memory"]; got != "729096" {
		t.Errorf("Memory.used_memory = %q, want 729096", got)
	}

	if got := info.Int("connected_clients"); got != 3 {
		t.Errorf("connected_clients = %d, want 3", got)
	}

	if got := info.Float("mem_fragmentation_ratio"); got != 1.07 {
		t.Errorf("mem_fragmentation_ratio = %v, want 1.07", got)
	}

	if !info.Bool("aof_enabled") {
		t.Error("aof_enabled = false, want true")
	}

	if got := info.Str("missing_field", "—"); got != "—" {
		t.Errorf("fallback = %q, want the em dash", got)
	}

	if got := info.Int("redis_version"); got != 0 {
		t.Errorf("Int on a non-numeric field = %d, want 0", got)
	}
}

func TestParseInfoKeyspaceLines(t *testing.T) {
	info := ParseInfo(sampleInfo)

	if len(info.Keyspace) != 2 {
		t.Fatalf("got %d keyspace rows, want 2", len(info.Keyspace))
	}

	// Sorted by database index, whatever order INFO reported them in.
	if info.Keyspace[0].DB != 0 || info.Keyspace[1].DB != 3 {
		t.Errorf("keyspace rows are not sorted by db: %+v", info.Keyspace)
	}

	first := info.Keyspace[0]

	if first.Keys != 35 || first.Expires != 28 || first.AvgTTL != 42426211 {
		t.Errorf("db0 = %+v, want keys=35 expires=28 avg_ttl=42426211", first)
	}

	// A keyspace line is a record, not a counter, so it must stay out of the flat
	// bag where Int("db0") would be meaningless.
	if _, present := info.Flat["db0"]; present {
		t.Error("db0 leaked into the flat field bag")
	}
}

func TestParseInfoIgnoresJunk(t *testing.T) {
	info := ParseInfo("# Server\nredis_version:7.2.4\nno-colon-here\n\n# Keyspace\ndbX:keys=1\ndb1:keys=notanumber,expires=2\n")

	if got := info.Str("redis_version", ""); got != "7.2.4" {
		t.Errorf("redis_version = %q, want 7.2.4", got)
	}

	if len(info.Keyspace) != 1 {
		t.Fatalf("got %d keyspace rows, want 1 (dbX is not a database)", len(info.Keyspace))
	}

	// An unparseable field is skipped rather than poisoning the whole row.
	if info.Keyspace[0].Keys != 0 || info.Keyspace[0].Expires != 2 {
		t.Errorf("db1 = %+v, want keys=0 expires=2", info.Keyspace[0])
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		value interface{}
		want  int64
		ok    bool
	}{
		{int64(42), 42, true},
		{int(42), 42, true},
		{float64(42.9), 42, true},
		{"42", 42, true},
		{[]byte("42"), 42, true},
		{nil, 0, false},
		{"not a number", 0, false},
		{struct{}{}, 0, false},
	}

	for _, testCase := range cases {
		got, ok := ToInt64(testCase.value)

		if got != testCase.want || ok != testCase.ok {
			t.Errorf("ToInt64(%#v) = (%d, %v), want (%d, %v)", testCase.value, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestToString(t *testing.T) {
	cases := []struct {
		value interface{}
		want  string
	}{
		{"hello", "hello"},
		{[]byte("hello"), "hello"},
		{int64(7), "7"},
		{1.5, "1.5"},
		{true, "1"},
		{nil, ""},
	}

	for _, testCase := range cases {
		if got := ToString(testCase.value); got != testCase.want {
			t.Errorf("ToString(%#v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}
