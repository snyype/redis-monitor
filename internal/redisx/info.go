package redisx

import (
	"sort"
	"strconv"
	"strings"
)

// KeyspaceDB is one line of the INFO Keyspace section:
// "db0:keys=41,expires=12,avg_ttl=0".
type KeyspaceDB struct {
	DB      int   `json:"db"`
	Keys    int64 `json:"keys"`
	Expires int64 `json:"expires"`
	AvgTTL  int64 `json:"avg_ttl"`
}

// Info is a parsed INFO reply.
//
// Both a section view and one flat bag are kept. The flat bag is what almost every
// caller wants — none of the counters this monitor reads collide across sections —
// while the sections stay available for anything that needs to know where a field
// came from.
type Info struct {
	Sections map[string]map[string]string
	Flat     map[string]string
	Keyspace []KeyspaceDB
}

// ParseInfo turns a raw INFO reply into sections, a flat field bag, and the
// keyspace lines. Keyspace lines are kept out of the flat bag: "db0" is a record,
// not a counter.
func ParseInfo(raw string) *Info {
	info := &Info{
		Sections: make(map[string]map[string]string),
		Flat:     make(map[string]string),
	}

	section := "Server"

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}

		field = strings.TrimSpace(field)
		value = strings.TrimSpace(value)

		if info.Sections[section] == nil {
			info.Sections[section] = make(map[string]string)
		}

		info.Sections[section][field] = value

		if entry, ok := parseKeyspaceLine(field, value); ok {
			info.Keyspace = append(info.Keyspace, entry)

			continue
		}

		info.Flat[field] = value
	}

	sort.Slice(info.Keyspace, func(a, b int) bool {
		return info.Keyspace[a].DB < info.Keyspace[b].DB
	})

	return info
}

func parseKeyspaceLine(field, value string) (KeyspaceDB, bool) {
	if !strings.HasPrefix(field, "db") {
		return KeyspaceDB{}, false
	}

	db, err := strconv.Atoi(strings.TrimPrefix(field, "db"))
	if err != nil {
		return KeyspaceDB{}, false
	}

	entry := KeyspaceDB{DB: db}

	for _, part := range strings.Split(value, ",") {
		name, raw, found := strings.Cut(part, "=")
		if !found {
			continue
		}

		parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			continue
		}

		switch strings.TrimSpace(name) {
		case "keys":
			entry.Keys = parsed
		case "expires":
			entry.Expires = parsed
		case "avg_ttl":
			entry.AvgTTL = parsed
		}
	}

	return entry, true
}

// Str returns a field or the given fallback.
func (i *Info) Str(field, fallback string) string {
	if value, ok := i.Flat[field]; ok && value != "" {
		return value
	}

	return fallback
}

// Int returns a field as an integer, 0 when absent or unparseable.
func (i *Info) Int(field string) int64 {
	parsed, err := strconv.ParseInt(i.Flat[field], 10, 64)
	if err != nil {
		return 0
	}

	return parsed
}

// Float returns a field as a float, 0 when absent or unparseable.
func (i *Info) Float(field string) float64 {
	parsed, err := strconv.ParseFloat(i.Flat[field], 64)
	if err != nil {
		return 0
	}

	return parsed
}

// Bool reads an INFO flag, which Redis writes as 0 or 1.
func (i *Info) Bool(field string) bool {
	return i.Int(field) == 1
}
