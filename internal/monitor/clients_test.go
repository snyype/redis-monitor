package monitor

import (
	"testing"

	"redismonitor/internal/redisx"
)

// redis5List is real CLIENT LIST output from Redis 5.0.14: no laddr, tot-mem,
// user, resp, watch or lib-name.
const redis5List = "id=10 addr=127.0.0.1:57540 fd=12 name= age=935 idle=88 flags=N db=0 sub=0 psub=0 multi=-1 qbuf=0 qbuf-free=0 obl=0 oll=0 omem=0 events=r cmd=ttl\n" +
	"id=11 addr=127.0.0.1:63904 fd=9 name=redis-monitor age=640 idle=27 flags=N db=1 sub=0 psub=0 multi=-1 qbuf=0 qbuf-free=0 obl=0 oll=0 omem=0 events=r cmd=scan\n" +
	"id=16 addr=127.0.0.1:59159 fd=13 name=predis age=445 idle=445 flags=N db=1 sub=2 psub=1 multi=3 qbuf=0 qbuf-free=0 obl=0 oll=0 omem=512 events=r cmd=get\n"

// redis7List carries the fields Redis 5 does not report.
const redis7List = "id=42 addr=10.0.0.9:52310 laddr=10.0.0.1:6379 fd=8 name=app age=100 idle=0 flags=O db=3 sub=0 psub=0 ssub=0 multi=-1 watch=4 qbuf=26 qbuf-free=20448 argv-mem=10 tot-mem=20512 events=r cmd=client|list user=default redir=-1 resp=3 lib-name=jedis lib-ver=5.1.0\n"

func TestParseClientListRedis5(t *testing.T) {
	rows, fields := parseClientList(redis5List)

	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	first := rows[0]

	if first.ID != 10 || first.Addr != "127.0.0.1:57540" || first.FD != 12 {
		t.Errorf("row 0 = %+v, want id 10 at 127.0.0.1:57540 on fd 12", first)
	}

	if first.Host != "127.0.0.1" || first.Port != "57540" {
		t.Errorf("addr split = %q / %q, want 127.0.0.1 / 57540", first.Host, first.Port)
	}

	if first.Age != 935 || first.Idle != 88 {
		t.Errorf("age/idle = %d/%d, want 935/88", first.Age, first.Idle)
	}

	if first.AgeLabel != "15m 35s" || first.IdleLabel != "1m 28s" {
		t.Errorf("labels = %q / %q", first.AgeLabel, first.IdleLabel)
	}

	if first.DB != 0 || first.Cmd != "ttl" || first.Events != "r" {
		t.Errorf("row 0 = %+v", first)
	}

	// A key detail: the monitor's own connections must be identifiable, or the
	// operator sees traffic they cannot account for.
	if !rows[1].Self {
		t.Error("the connection named redis-monitor was not marked as our own")
	}

	if rows[0].Self || rows[2].Self {
		t.Error("a connection this monitor did not open was marked as its own")
	}

	third := rows[2]

	if third.Sub != 2 || third.PSub != 1 || third.Multi != 3 || third.OMem != 512 {
		t.Errorf("row 2 = %+v, want sub 2 psub 1 multi 3 omem 512", third)
	}

	if third.OMemHuman != "512 B" {
		t.Errorf("omem_human = %q, want 512 B", third.OMemHuman)
	}

	// Nothing this server never sent should be claimed as available.
	if fields.LocalAddr || fields.TotMem || fields.User || fields.Resp || fields.Watch || fields.Lib {
		t.Errorf("fields = %+v, want every optional field absent on Redis 5", fields)
	}

	if third.TotMem != nil || third.User != nil || third.Resp != nil || third.Watch != nil {
		t.Errorf("row 2 = %+v, want nil for fields this server does not report", third)
	}
}

func TestParseClientListRedis7Fields(t *testing.T) {
	rows, fields := parseClientList(redis7List)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}

	row := rows[0]

	if !fields.LocalAddr || !fields.TotMem || !fields.User || !fields.Resp || !fields.Watch || !fields.Lib {
		t.Errorf("fields = %+v, want every optional field present", fields)
	}

	if row.LocalAddr != "10.0.0.1:6379" {
		t.Errorf("laddr = %q", row.LocalAddr)
	}

	if row.TotMem == nil || *row.TotMem != 20512 {
		t.Errorf("tot_mem = %v, want 20512", row.TotMem)
	}

	if row.TotMemHuman == nil || *row.TotMemHuman != "20 KB" {
		t.Errorf("tot_mem_human = %v, want 20 KB", row.TotMemHuman)
	}

	if row.Watch == nil || *row.Watch != 4 {
		t.Errorf("watch = %v, want 4", row.Watch)
	}

	if row.User == nil || *row.User != "default" {
		t.Errorf("user = %v, want default", row.User)
	}

	if row.Resp == nil || *row.Resp != 3 {
		t.Errorf("resp = %v, want 3", row.Resp)
	}

	// lib-name and lib-ver arrive as two fields and read better as one column.
	if row.Lib != "jedis 5.1.0" {
		t.Errorf("lib = %q, want \"jedis 5.1.0\"", row.Lib)
	}

	// Unknown fields (ssub, argv-mem, redir, qbuf-free) must be ignored rather than
	// shifting anything, which is why parsing is field-name driven.
	if row.ID != 42 || row.Cmd != "client|list" || row.DB != 3 {
		t.Errorf("row = %+v, want id 42 cmd client|list db 3", row)
	}
}

func TestParseClientListFlags(t *testing.T) {
	rows, _ := parseClientList(redis7List)

	// "O" is a MONITOR client — worth spelling out, since the letter says nothing.
	if got := rows[0].FlagLabels; len(got) != 1 || got[0] != "monitor" {
		t.Errorf("flag labels = %v, want [monitor]", got)
	}

	rows, _ = parseClientList("id=1 addr=x:1 flags=Nx db=0 multi=2 cmd=exec\n")

	if got := rows[0].FlagLabels; len(got) != 2 || got[0] != "normal" || got[1] != "in MULTI" {
		t.Errorf("flag labels = %v, want [normal, in MULTI]", got)
	}

	// An unrecognised letter from a future Redis is passed through rather than
	// dropped, so the column never silently loses information.
	rows, _ = parseClientList("id=1 addr=x:1 flags=NZ db=0 cmd=ping\n")

	if got := rows[0].FlagLabels; len(got) != 2 || got[1] != "Z" {
		t.Errorf("flag labels = %v, want the unknown letter kept", got)
	}
}

func TestParseClientListIgnoresJunk(t *testing.T) {
	rows, _ := parseClientList("\n   \nnot-a-client-line\nid=7 addr=1.2.3.4:9 db=2 cmd=ping\n")

	// Blank lines and a line with no key=value pair at all yield no row, so a
	// malformed reply cannot pad the table with empty connections.
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want only the one real connection", len(rows))
	}

	if rows[0].ID != 7 || rows[0].DB != 2 {
		t.Errorf("row = %+v, want id 7 db 2", rows[0])
	}
}

func TestParseClientListIPv6Address(t *testing.T) {
	rows, _ := parseClientList("id=1 addr=[::1]:6379 db=0 cmd=ping\n")

	// Split on the LAST colon, so an IPv6 address keeps its own.
	if rows[0].Host != "[::1]" || rows[0].Port != "6379" {
		t.Errorf("addr split = %q / %q, want [::1] / 6379", rows[0].Host, rows[0].Port)
	}
}

func TestSummariseClients(t *testing.T) {
	rows, _ := parseClientList(redis5List)

	serverInfo := redisx.ParseInfo("# Clients\nconnected_clients:4\nblocked_clients:1\nclient_recent_max_input_buffer:2\n")

	// 10000 stands in for whatever maxClients resolved, whether that came from INFO
	// (Redis 7 reports it) or the CONFIG GET fallback (anything older does not).
	totals := summariseClients(rows, serverInfo, 10000)

	// Connected comes from INFO and is the server's own count; Listed is how many
	// rows we parsed. They differ, and the payload keeps both.
	if totals.Connected != 4 {
		t.Errorf("connected = %d, want 4 from INFO", totals.Connected)
	}

	if totals.Blocked != 1 || totals.MaxClients != 10000 {
		t.Errorf("totals = %+v", totals)
	}

	if totals.UsedPercent == nil || *totals.UsedPercent != 0 {
		t.Errorf("used_percent = %v, want 0 (4 of 10000 rounds to 0.0)", totals.UsedPercent)
	}

	if totals.Databases != 2 {
		t.Errorf("databases = %d, want 2 (db0 and db1)", totals.Databases)
	}

	if totals.Subscriptions != 3 {
		t.Errorf("subscriptions = %d, want 3 (sub 2 + psub 1)", totals.Subscriptions)
	}

	if totals.InTransaction != 1 {
		t.Errorf("in_transaction = %d, want 1 (only multi >= 0 counts)", totals.InTransaction)
	}

	if totals.OwnConnections != 1 {
		t.Errorf("own_connections = %d, want 1", totals.OwnConnections)
	}

	if totals.OutputMemory != 512 || totals.OutputMemoryHuman != "512 B" {
		t.Errorf("output memory = %d / %q", totals.OutputMemory, totals.OutputMemoryHuman)
	}

	// tot-mem is absent on Redis 5, so the total must be nil rather than a
	// misleading zero.
	if totals.TotalMemory != nil {
		t.Errorf("total_memory = %v, want nil when the server does not report tot-mem", *totals.TotalMemory)
	}

	if totals.LongestIdle != 445 || totals.OldestAge != 935 {
		t.Errorf("longest idle / oldest age = %d / %d, want 445 / 935", totals.LongestIdle, totals.OldestAge)
	}
}

func TestClientChartsBucketEveryConnection(t *testing.T) {
	rows, _ := parseClientList(redis5List)
	charts := clientCharts(rows)

	total := func(chart []CountRow) int64 {
		var sum int64
		for _, row := range chart {
			sum += row.Count
		}

		return sum
	}

	if got := total(charts.ByDatabase); got != 3 {
		t.Errorf("by_database sums to %d, want every one of the 3 connections", got)
	}

	if got := total(charts.IdleBuckets); got != 3 {
		t.Errorf("idle_buckets sums to %d, want 3", got)
	}

	if got := total(charts.ByCommand); got != 3 {
		t.Errorf("by_command sums to %d, want 3", got)
	}

	// An unnamed connection still needs a bar, or the chart quietly loses it.
	var unnamed int64
	for _, row := range charts.ByName {
		if row.Label == "(unnamed)" {
			unnamed = row.Count
		}
	}

	if unnamed != 1 {
		t.Errorf("(unnamed) count = %d, want 1", unnamed)
	}
}
