package monitor

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/redisx"
)

// Clients is the connected-clients page.
//
// Unlike everything else on this dashboard, this is server-wide rather than
// per-database: CLIENT LIST reports every connection to the instance, and each row
// carries the database it happens to be on. It is also exact rather than sampled —
// there is no SCAN involved — but it is a snapshot of a list that changes between
// one request and the next.
type Clients struct {
	GeneratedAt int64        `json:"generated_at"`
	Totals      ClientTotals `json:"totals"`
	Rows        []ClientRow  `json:"rows"`
	Charts      ClientCharts `json:"charts"`
	// Fields says which optional CLIENT LIST fields this server reports, so the UI
	// can drop columns rather than render a wall of dashes. Redis 5 has no laddr,
	// tot-mem, user, resp, watch or lib-name; Redis 7 has them all.
	Fields ClientFields `json:"fields"`
	// Truncated is true when there were more connections than max_clients, so the
	// table is a prefix of the list rather than all of it.
	Truncated bool `json:"truncated"`
	Listed    int  `json:"listed"`
}

type ClientTotals struct {
	// Connected and Blocked come from INFO and are the server's own count; Listed
	// is how many rows this payload carries. They differ when the list is capped,
	// or simply because a connection opened between the two commands.
	Connected             int64    `json:"connected"`
	Blocked               int64    `json:"blocked"`
	MaxClients            int64    `json:"maxclients"`
	UsedPercent           *float64 `json:"used_percent"`
	RecentMaxInputBuffer  int64    `json:"recent_max_input_buffer"`
	RecentMaxOutputBuffer int64    `json:"recent_max_output_buffer"`

	Databases      int   `json:"databases"`
	Subscriptions  int64 `json:"subscriptions"`
	InTransaction  int   `json:"in_transaction"`
	Watching       int   `json:"watching"`
	Monitors       int   `json:"monitors"`
	Replicas       int   `json:"replicas"`
	OwnConnections int   `json:"own_connections"`

	OutputMemory      int64   `json:"output_memory"`
	OutputMemoryHuman string  `json:"output_memory_human"`
	TotalMemory       *int64  `json:"total_memory"`
	TotalMemoryHuman  *string `json:"total_memory_human"`

	LongestIdle      int64  `json:"longest_idle"`
	LongestIdleLabel string `json:"longest_idle_label"`
	OldestAge        int64  `json:"oldest_age"`
	OldestAgeLabel   string `json:"oldest_age_label"`
}

// ClientRow is one connection.
type ClientRow struct {
	ID        int64  `json:"id"`
	Addr      string `json:"addr"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	LocalAddr string `json:"laddr"`
	Name      string `json:"name"`
	FD        int64  `json:"fd"`

	Age       int64  `json:"age"`
	AgeLabel  string `json:"age_label"`
	Idle      int64  `json:"idle"`
	IdleLabel string `json:"idle_label"`

	Flags string `json:"flags"`
	// FlagLabels spells the flag letters out; "N" on its own means nothing more
	// than "an ordinary client", which is not obvious from the letter.
	FlagLabels []string `json:"flag_labels"`

	DB    int    `json:"db"`
	Sub   int64  `json:"sub"`
	PSub  int64  `json:"psub"`
	Multi int64  `json:"multi"`
	Watch *int64 `json:"watch"`

	QBuf        int64   `json:"qbuf"`
	OMem        int64   `json:"omem"`
	OMemHuman   string  `json:"omem_human"`
	TotMem      *int64  `json:"tot_mem"`
	TotMemHuman *string `json:"tot_mem_human"`

	Events string  `json:"events"`
	Cmd    string  `json:"cmd"`
	User   *string `json:"user"`
	Resp   *int64  `json:"resp"`
	Lib    string  `json:"lib"`

	// Self marks the monitor's own connections, named via CLIENT SETNAME at dial
	// time. Without this the operator sees traffic they cannot account for and
	// wonders who is scanning their keyspace.
	Self bool `json:"self"`
}

type ClientCharts struct {
	ByDatabase  []CountRow `json:"by_database"`
	ByCommand   []CountRow `json:"by_command"`
	ByName      []CountRow `json:"by_name"`
	ByFlag      []CountRow `json:"by_flag"`
	IdleBuckets []CountRow `json:"idle_buckets"`
}

type ClientFields struct {
	LocalAddr bool `json:"laddr"`
	TotMem    bool `json:"tot_mem"`
	User      bool `json:"user"`
	Resp      bool `json:"resp"`
	Watch     bool `json:"watch"`
	Lib       bool `json:"lib"`
}

// flagNames spells out the CLIENT LIST flag letters.
var flagNames = map[byte]string{
	'N': "normal",
	'M': "master",
	'S': "replica",
	'O': "monitor",
	'x': "in MULTI",
	'b': "blocked",
	'd': "dirty MULTI",
	'c': "closing after reply",
	'u': "unblocked",
	'A': "closing ASAP",
	'U': "unix socket",
	'r': "cluster read-only",
	't': "tracking",
	'T': "tracking broken redirect",
	'R': "tracking broken",
	'B': "broadcast tracking",
	'e': "no-evict",
	'i': "no-touch",
}

// Clients reads the connection list, cached briefly. The cache exists because this
// is the one page an operator refreshes compulsively during an incident, and
// CLIENT LIST walks every connection on the server each time.
func (s *Service) Clients(ctx context.Context, db int, fresh bool) (*Clients, error) {
	key := cacheKey("clients", db)
	ttl := time.Duration(s.cfg.ClientsCacheSeconds) * time.Second

	if !fresh && ttl > 0 {
		if cached, ok := s.cache.Get(key); ok {
			if clients, ok := cached.(*Clients); ok {
				return clients, nil
			}
		}
	}

	clients, err := s.buildClients(ctx, db)
	if err != nil {
		return nil, err
	}

	s.cache.Put(key, clients, ttl)

	return clients, nil
}

func (s *Service) buildClients(ctx context.Context, db int) (*Clients, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	// CLIENT LIST is the one command here that an ACL can withhold without the
	// whole connection being unusable, so its error is returned as-is: the page
	// says why it is empty rather than pretending there are no clients.
	raw, err := client.ClientList(ctx).Result()
	if err != nil {
		return nil, err
	}

	serverInfo, err := info(ctx, client)
	if err != nil {
		return nil, err
	}

	rows, fields := parseClientList(raw)

	result := &Clients{
		GeneratedAt: s.now().Unix(),
		Fields:      fields,
		Listed:      len(rows),
	}

	result.Totals = summariseClients(rows, serverInfo, maxClients(ctx, client, serverInfo))
	result.Charts = clientCharts(rows)

	// Longest-lived first: on an ops screen the connection that has been sitting
	// there since the last deploy is the interesting one.
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].Age != rows[b].Age {
			return rows[a].Age > rows[b].Age
		}

		return rows[a].ID < rows[b].ID
	})

	if len(rows) > s.cfg.MaxClients {
		rows = rows[:s.cfg.MaxClients]
		result.Truncated = true
	}

	result.Rows = rows

	return result, nil
}

// parseClientList reads the CLIENT LIST reply.
//
// Deliberately field-name driven rather than positional: the field set grew across
// Redis versions (laddr, tot-mem, user, resp, lib-name and watch are all newer than
// Redis 5), and an unknown field must be ignored rather than shifting everything
// after it.
func parseClientList(raw string) ([]ClientRow, ClientFields) {
	var (
		rows   []ClientRow
		fields ClientFields
	)

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		row := ClientRow{DB: 0, Multi: -1}
		seen := false

		for _, pair := range strings.Fields(line) {
			name, value, found := strings.Cut(pair, "=")
			if !found {
				continue
			}

			seen = true

			switch name {
			case "id":
				row.ID = toInt(value)
			case "addr":
				row.Addr = value
				row.Host, row.Port = splitAddr(value)
			case "laddr":
				row.LocalAddr = value
				fields.LocalAddr = true
			case "name":
				row.Name = value
			case "fd":
				row.FD = toInt(value)
			case "age":
				row.Age = toInt(value)
			case "idle":
				row.Idle = toInt(value)
			case "flags":
				row.Flags = value
				row.FlagLabels = describeFlags(value)
			case "db":
				row.DB = int(toInt(value))
			case "sub":
				row.Sub = toInt(value)
			case "psub":
				row.PSub = toInt(value)
			case "multi":
				row.Multi = toInt(value)
			case "watch":
				row.Watch = int64Ptr(toInt(value))
				fields.Watch = true
			case "qbuf":
				row.QBuf = toInt(value)
			case "omem":
				row.OMem = toInt(value)
			case "tot-mem":
				row.TotMem = int64Ptr(toInt(value))
				fields.TotMem = true
			case "events":
				row.Events = value
			case "cmd":
				row.Cmd = value
			case "user":
				user := value
				row.User = &user
				fields.User = true
			case "resp":
				row.Resp = int64Ptr(toInt(value))
				fields.Resp = true
			case "lib-name":
				if value != "" {
					row.Lib = value
					fields.Lib = true
				}
			case "lib-ver":
				if value != "" && row.Lib != "" {
					row.Lib += " " + value
					fields.Lib = true
				}
			}
		}

		if !seen {
			continue
		}

		row.AgeLabel = ttlLabel(row.Age)
		row.IdleLabel = ttlLabel(row.Idle)
		row.OMemHuman = humanBytes(row.OMem)
		row.Self = row.Name == redisx.ClientName

		if row.TotMem != nil {
			row.TotMemHuman = humanPtr(*row.TotMem)
		}

		rows = append(rows, row)
	}

	return rows, fields
}

// maxClients is the connection ceiling.
//
// Redis only started reporting maxclients in INFO with 7.0; before that it is a
// CONFIG parameter and nothing else. Without this fallback the page would report
// "no maxclients limit" on Redis 5, which is wrong — there is a limit (10000 by
// default), it just cannot be read from INFO. CONFIG GET can itself be renamed or
// withheld on managed offerings, so a failure means "unknown" rather than "none".
func maxClients(ctx context.Context, client interface {
	ConfigGet(ctx context.Context, parameter string) *redis.MapStringStringCmd
}, serverInfo *redisx.Info) int64 {
	if reported := serverInfo.Int("maxclients"); reported > 0 {
		return reported
	}

	values, err := client.ConfigGet(ctx, "maxclients").Result()
	if err != nil {
		return 0
	}

	parsed, err := strconv.ParseInt(values["maxclients"], 10, 64)
	if err != nil {
		return 0
	}

	return parsed
}

func summariseClients(rows []ClientRow, serverInfo *redisx.Info, ceiling int64) ClientTotals {
	connected := serverInfo.Int("connected_clients")

	totals := ClientTotals{
		Connected:             connected,
		Blocked:               serverInfo.Int("blocked_clients"),
		MaxClients:            ceiling,
		UsedPercent:           percent(connected, ceiling),
		RecentMaxInputBuffer:  serverInfo.Int("client_recent_max_input_buffer"),
		RecentMaxOutputBuffer: serverInfo.Int("client_recent_max_output_buffer"),
	}

	databases := make(map[int]bool)
	var totalMemory int64
	haveTotalMemory := false

	for _, row := range rows {
		databases[row.DB] = true

		totals.Subscriptions += row.Sub + row.PSub
		totals.OutputMemory += row.OMem

		if row.Multi >= 0 {
			totals.InTransaction++
		}

		if row.Watch != nil && *row.Watch > 0 {
			totals.Watching++
		}

		if strings.ContainsRune(row.Flags, 'O') {
			totals.Monitors++
		}

		if strings.ContainsRune(row.Flags, 'S') {
			totals.Replicas++
		}

		if row.Self {
			totals.OwnConnections++
		}

		if row.TotMem != nil {
			totalMemory += *row.TotMem
			haveTotalMemory = true
		}

		if row.Idle > totals.LongestIdle {
			totals.LongestIdle = row.Idle
		}

		if row.Age > totals.OldestAge {
			totals.OldestAge = row.Age
		}
	}

	totals.Databases = len(databases)
	totals.OutputMemoryHuman = humanBytes(totals.OutputMemory)
	totals.LongestIdleLabel = ttlLabel(totals.LongestIdle)
	totals.OldestAgeLabel = ttlLabel(totals.OldestAge)

	if haveTotalMemory {
		totals.TotalMemory = &totalMemory
		totals.TotalMemoryHuman = humanPtr(totalMemory)
	}

	return totals
}

func clientCharts(rows []ClientRow) ClientCharts {
	byDatabase := make(map[string]int64)
	byCommand := make(map[string]int64)
	byName := make(map[string]int64)
	byFlag := make(map[string]int64)
	idleBuckets := make(map[string]int64)

	for _, row := range rows {
		byDatabase["db"+strconv.Itoa(row.DB)]++

		command := row.Cmd
		if command == "" {
			command = "(none yet)"
		}
		byCommand[command]++

		name := row.Name
		if name == "" {
			name = "(unnamed)"
		}
		byName[name]++

		for _, label := range row.FlagLabels {
			byFlag[label]++
		}

		idleBuckets[bucketLabel(row.Idle, IdleBuckets)]++
	}

	return ClientCharts{
		ByDatabase:  countRows(byDatabase),
		ByCommand:   topRows(byCommand, 10),
		ByName:      topRows(byName, 10),
		ByFlag:      countRows(byFlag),
		IdleBuckets: bucketRows(idleBuckets, IdleBuckets),
	}
}

func describeFlags(flags string) []string {
	labels := make([]string, 0, len(flags))

	for index := 0; index < len(flags); index++ {
		if name, ok := flagNames[flags[index]]; ok {
			labels = append(labels, name)

			continue
		}

		labels = append(labels, string(flags[index]))
	}

	return labels
}

// splitAddr separates host from port on the last colon, so an IPv6 address keeps
// its own colons.
func splitAddr(addr string) (string, string) {
	index := strings.LastIndex(addr, ":")
	if index < 0 {
		return addr, ""
	}

	return addr[:index], addr[index+1:]
}

func toInt(value string) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}

	return parsed
}
