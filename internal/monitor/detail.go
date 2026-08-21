package monitor

import (
	"context"
	"encoding/json"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/redisx"
)

// KeyDetail is everything the drawer shows for one key, including a capped preview
// of its value.
type KeyDetail struct {
	KeyRow

	Exists   bool     `json:"exists"`
	Database int      `json:"database"`
	Encoding *string  `json:"encoding"`
	IdleTime *int64   `json:"idle_time"`
	Preview  *Preview `json:"preview"`
}

// Preview is a bounded read of a value. Which field carries the payload depends on
// Kind, which mirrors the Redis type:
//
//	string, stream   → Value
//	list, set        → Items
//	hash, zset       → Pairs
//	redacted         → nothing at all, only Note
//	unsupported      → nothing at all, only Note
//
// Shown and Total are always both present, so the UI can say "100 of 12,480"
// rather than implying it has the whole value.
type Preview struct {
	Kind      string   `json:"kind"`
	Note      string   `json:"note,omitempty"`
	Value     string   `json:"value,omitempty"`
	Items     []string `json:"items,omitempty"`
	Pairs     []Pair   `json:"pairs,omitempty"`
	Truncated bool     `json:"truncated"`
	Shown     int64    `json:"shown"`
	Total     int64    `json:"total"`
}

// KeyDetail reads one key. A key whose NAME matches a sensitive pattern is never
// read back at all: the row still carries its type, TTL and size, but the preview
// says only that it was withheld.
func (s *Service) KeyDetail(ctx context.Context, db int, key string) (*KeyDetail, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	keyType, err := client.Type(ctx, key).Result()
	if err != nil {
		return nil, err
	}

	if keyType == "none" || keyType == "" {
		return &KeyDetail{
			KeyRow:   KeyRow{Key: key, Prefix: prefixOf(key), TTL: -2, TTLLabel: ttlLabel(-2)},
			Exists:   false,
			Database: db,
		}, nil
	}

	rows, err := s.describeKeys(ctx, client, []string{key}, []string{keyType})
	if err != nil {
		return nil, err
	}

	if len(rows) == 0 {
		return &KeyDetail{
			KeyRow:   KeyRow{Key: key, Prefix: prefixOf(key), TTL: -2, TTLLabel: ttlLabel(-2)},
			Exists:   false,
			Database: db,
		}, nil
	}

	detail := &KeyDetail{
		KeyRow:   rows[0],
		Exists:   true,
		Database: db,
	}

	if encoding, ok := redisx.ObjectEncoding(ctx, client, key); ok {
		detail.Encoding = &encoding
	}

	if idle, ok := redisx.ObjectIdleTime(ctx, client, key); ok {
		detail.IdleTime = int64Ptr(idle)
	}

	if detail.Redacted {
		detail.Preview = &Preview{
			Kind: "redacted",
			Note: "This key name matches a sensitive pattern, so its value is not returned.",
		}

		return detail, nil
	}

	preview, err := s.valuePreview(ctx, client, key, keyType)
	if err != nil {
		return nil, err
	}

	detail.Preview = preview

	return detail, nil
}

// valuePreview reads a bounded slice of a value: strings cut at preview_bytes,
// collections at preview_elements, everything passed through value redaction.
//
// Never read a whole value. A single cached response can be megabytes, and the
// point of this screen is to see what a key holds, not to move it over the wire.
func (s *Service) valuePreview(ctx context.Context, client *redis.Client, key, keyType string) (*Preview, error) {
	limitBytes := int64(s.cfg.PreviewBytes)
	limitElements := int64(s.cfg.PreviewElements)

	switch keyType {
	case "string":
		total, _ := redisx.OptionalInt(ctx, client, "STRLEN", key)

		value, err := client.GetRange(ctx, key, 0, limitBytes-1).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		return &Preview{
			Kind:      "string",
			Value:     s.redact.Value(value),
			Truncated: total > limitBytes,
			Shown:     minInt64(total, limitBytes),
			Total:     total,
		}, nil

	case "list":
		total, _ := redisx.OptionalInt(ctx, client, "LLEN", key)

		items, err := client.LRange(ctx, key, 0, limitElements-1).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		return s.listPreview("list", items, total), nil

	case "set":
		total, _ := redisx.OptionalInt(ctx, client, "SCARD", key)

		// SSCAN rather than SMEMBERS: a COUNT-bounded page cannot pull a
		// million-member set into memory the way SMEMBERS would.
		items, _, err := client.SScan(ctx, key, 0, "", limitElements).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		return s.listPreview("set", items, total), nil

	case "hash":
		total, _ := redisx.OptionalInt(ctx, client, "HLEN", key)

		flat, _, err := client.HScan(ctx, key, 0, "", limitElements).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		pairs := make([]Pair, 0, len(flat)/2)

		for index := 0; index+1 < len(flat) && int64(len(pairs)) < limitElements; index += 2 {
			pairs = append(pairs, s.redact.Pair(flat[index], flat[index+1]))
		}

		return &Preview{
			Kind:      "hash",
			Pairs:     pairs,
			Truncated: total > int64(len(pairs)),
			Shown:     int64(len(pairs)),
			Total:     total,
		}, nil

	case "zset":
		total, _ := redisx.OptionalInt(ctx, client, "ZCARD", key)

		members, err := client.ZRangeWithScores(ctx, key, 0, limitElements-1).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		pairs := make([]Pair, 0, len(members))

		for _, member := range members {
			// Scores are numbers, so only the member name needs masking.
			pairs = append(pairs, Pair{
				Field: s.redact.Value(redisx.ToString(member.Member)),
				Value: formatScore(member.Score),
			})
		}

		return &Preview{
			Kind:      "zset",
			Pairs:     pairs,
			Truncated: total > int64(len(pairs)),
			Shown:     int64(len(pairs)),
			Total:     total,
		}, nil

	case "stream":
		total, _ := redisx.OptionalInt(ctx, client, "XLEN", key)

		entries, err := client.XRangeN(ctx, key, "-", "+", limitElements).Result()
		if err != nil && err != redis.Nil {
			return nil, err
		}

		encoded, err := json.MarshalIndent(streamEntries(entries), "", "  ")
		if err != nil {
			return nil, err
		}

		return &Preview{
			Kind:      "stream",
			Value:     s.redact.Value(string(encoded)),
			Truncated: total > int64(len(entries)),
			Shown:     int64(len(entries)),
			Total:     total,
		}, nil

	default:
		return &Preview{
			Kind: "unsupported",
			Note: `No preview available for type "` + keyType + `".`,
		}, nil
	}
}

func (s *Service) listPreview(kind string, items []string, total int64) *Preview {
	if int64(len(items)) > int64(s.cfg.PreviewElements) {
		items = items[:s.cfg.PreviewElements]
	}

	masked := s.redact.Values(items)

	return &Preview{
		Kind:      kind,
		Items:     masked,
		Truncated: total > int64(len(masked)),
		Shown:     int64(len(masked)),
		Total:     total,
	}
}

// streamEntry keeps a stream preview readable: the entry ID stays alongside its
// fields instead of being buried in a nested array.
type streamEntry struct {
	ID     string                 `json:"id"`
	Values map[string]interface{} `json:"values"`
}

func streamEntries(messages []redis.XMessage) []streamEntry {
	entries := make([]streamEntry, 0, len(messages))

	for _, message := range messages {
		entries = append(entries, streamEntry{ID: message.ID, Values: message.Values})
	}

	return entries
}

// formatScore renders a sorted-set score the way Redis reports it — no trailing
// zeros, no exponent for ordinary values.
func formatScore(score float64) string {
	encoded, err := json.Marshal(score)
	if err != nil {
		return ""
	}

	return string(encoded)
}
