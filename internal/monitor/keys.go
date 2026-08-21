package monitor

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/redisx"
)

// clientTypeFilterMaxBatches caps the walk when the type filter has to run on this
// side of the wire.
//
// The normal cap assumes a cheap batch — a sparse MATCH simply returns few keys. A
// client-side type filter instead pays a TYPE lookup on every key it throws away,
// so it gets a much shorter leash. The cursor travels back in the response, so
// "Load more" picks up exactly where this left off.
const clientTypeFilterMaxBatches = 20

// KeyPage is one page of the key browser.
type KeyPage struct {
	Rows        []KeyRow `json:"rows"`
	Cursor      string   `json:"cursor"`
	HasMore     bool     `json:"has_more"`
	ScanStopped bool     `json:"scan_stopped"`
	Database    int      `json:"database"`
	PerPage     int      `json:"per_page"`
	TotalKeys   int64    `json:"total_keys"`
	TypeFilter  string   `json:"type_filter,omitempty"`
	// ServerFiltered is false when the server could not apply the type filter
	// (SCAN ... TYPE needs Redis 6) and it was applied here instead.
	ServerFiltered bool `json:"server_filtered"`
}

// KeyRow is one row of the key table.
type KeyRow struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	TTL         int64    `json:"ttl"`
	TTLLabel    string   `json:"ttl_label"`
	Size        *KeySize `json:"size"`
	Memory      *int64   `json:"memory"`
	MemoryHuman *string  `json:"memory_human"`
	Prefix      string   `json:"prefix"`
	Redacted    bool     `json:"redacted"`
}

// KeySize is a byte length for a string and an element count for a collection —
// the unit travels with the number so the UI never has to guess.
type KeySize struct {
	Unit  string `json:"unit"`
	Value int64  `json:"value"`
}

// KeyQuery is one request for a page of keys.
type KeyQuery struct {
	Pattern string
	Type    string
	Cursor  string
	PerPage int
}

// Keys returns one page of keys, cursor-paginated exactly the way SCAN is: the
// returned cursor is opaque, hand it straight back for the next page, and a "0"
// cursor means the iteration is complete.
//
// per_page is a target rather than a promise. A SCAN batch has to be consumed
// whole — the cursor has already moved past it, so a dropped key would never be
// seen again — which means a page can come back slightly larger than asked for.
// Paging until has_more is false still covers the keyspace exactly once per pass.
func (s *Service) Keys(ctx context.Context, db int, query KeyQuery) (*KeyPage, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	perPage := minInt(atLeastInt(query.PerPage, 1), s.cfg.MaxPageSize)

	// With no filter, COUNT is the page size. With one, most of a batch gets
	// discarded, so ask for bigger batches and let the row count stop the loop.
	scanCount := int64(perPage)
	if query.Pattern != "" || query.Type != "" {
		scanCount = int64(maxInt64(int64(perPage), int64(s.cfg.ScanCount)))
	}

	cursor := parseCursor(query.Cursor)

	// SCAN ... TYPE only exists from Redis 6. On an older server the first attempt
	// errors, and from then on the filter is applied here instead — the type of
	// every row is read anyway.
	filterOnServer := query.Type != ""
	batchCap := s.cfg.MaxScanIterations

	page := &KeyPage{
		Rows:       []KeyRow{},
		Database:   db,
		PerPage:    perPage,
		TypeFilter: query.Type,
	}

	iterations := 0

	for {
		var (
			batch []string
			next  uint64
		)

		if filterOnServer {
			batch, next, err = client.ScanType(ctx, cursor, query.Pattern, scanCount, query.Type).Result()

			if err != nil {
				// Only a server that will not accept the TYPE argument justifies the
				// fallback; a dead connection has to stay an error.
				if !isReplyError(err) {
					return nil, err
				}

				filterOnServer = false
				batchCap = minInt(s.cfg.MaxScanIterations, clientTypeFilterMaxBatches)

				batch, next, err = client.Scan(ctx, cursor, query.Pattern, scanCount).Result()
				if err != nil {
					return nil, err
				}
			}
		} else {
			batch, next, err = client.Scan(ctx, cursor, query.Pattern, scanCount).Result()
			if err != nil {
				return nil, err
			}
		}

		cursor = next
		iterations++

		if len(batch) > 0 {
			types, err := s.resolveTypes(ctx, client, batch, query.Type, filterOnServer)
			if err != nil {
				return nil, err
			}

			batch, types = keepMatchingType(batch, types, query.Type, filterOnServer)

			rows, err := s.describeKeys(ctx, client, batch, types)
			if err != nil {
				return nil, err
			}

			page.Rows = append(page.Rows, rows...)
		}

		if cursor == 0 || len(page.Rows) >= perPage || iterations >= batchCap {
			break
		}
	}

	page.Cursor = strconv.FormatUint(cursor, 10)
	page.HasMore = cursor != 0
	page.ScanStopped = cursor != 0 && len(page.Rows) < perPage
	page.TotalKeys = dbSize(ctx, client)
	page.ServerFiltered = filterOnServer

	return page, nil
}

// resolveTypes gets the type of every key in a batch.
//
// When the server applied the filter there is nothing to ask: SCAN ... TYPE only
// returned keys of that type, which saves a whole round trip per batch.
func (s *Service) resolveTypes(
	ctx context.Context,
	client *redis.Client,
	batch []string,
	wantType string,
	filteredOnServer bool,
) ([]string, error) {
	if filteredOnServer {
		types := make([]string, len(batch))

		for index := range types {
			types[index] = wantType
		}

		return types, nil
	}

	cmds := make([]*redis.StatusCmd, len(batch))

	_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, key := range batch {
			cmds[index] = pipe.Type(ctx, key)
		}

		return nil
	})
	if err != nil && !isReplyError(err) {
		return nil, err
	}

	types := make([]string, len(batch))

	for index := range batch {
		if cmds[index] != nil && cmds[index].Err() == nil {
			types[index] = cmds[index].Val()
		}
	}

	return types, nil
}

// keepMatchingType drops the keys a client-side type filter rejects, before any
// further command is spent on them.
func keepMatchingType(batch, types []string, wantType string, filteredOnServer bool) ([]string, []string) {
	if wantType == "" || filteredOnServer {
		return batch, types
	}

	keptKeys := make([]string, 0, len(batch))
	keptTypes := make([]string, 0, len(types))

	for index, key := range batch {
		if types[index] == wantType {
			keptKeys = append(keptKeys, key)
			keptTypes = append(keptTypes, types[index])
		}
	}

	return keptKeys, keptTypes
}

// describeKeys fills in TTL, element count and memory footprint for a batch whose
// types are already known — one pipelined round trip for the whole batch.
//
// The size command depends on the type, which is why the type is resolved first:
// asking STRLEN of a hash is an error reply, and an error mid-pipeline would cost
// the batch its speedup.
func (s *Service) describeKeys(
	ctx context.Context,
	client *redis.Client,
	batch []string,
	types []string,
) ([]KeyRow, error) {
	if len(batch) == 0 {
		return nil, nil
	}

	ttls := make([]*redis.DurationCmd, len(batch))
	memories := make([]*redis.Cmd, len(batch))
	sizes := make([]*redis.Cmd, len(batch))

	_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, key := range batch {
			ttls[index] = pipe.TTL(ctx, key)
			memories[index] = pipe.Do(ctx, "MEMORY", "USAGE", key, "SAMPLES", 0)

			if command, unit := sizeCommand(types[index], key); unit != "" {
				sizes[index] = pipe.Do(ctx, command...)
			}
		}

		return nil
	})
	if err != nil && !isReplyError(err) {
		return nil, err
	}

	rows := make([]KeyRow, 0, len(batch))

	for index, key := range batch {
		row := KeyRow{
			Key:      key,
			Type:     types[index],
			TTL:      -1,
			Prefix:   prefixOf(key),
			Redacted: s.redact.IsSensitiveKey(key),
		}

		if ttls[index] != nil && ttls[index].Err() == nil {
			row.TTL = durationToTTL(ttls[index].Val())
		}

		row.TTLLabel = ttlLabel(row.TTL)

		if memory, ok := redisx.CmdInt64(memories[index]); ok {
			row.Memory = int64Ptr(memory)
			row.MemoryHuman = humanPtr(memory)
		}

		if _, unit := sizeCommand(types[index], key); unit != "" {
			if value, ok := redisx.CmdInt64(sizes[index]); ok {
				row.Size = &KeySize{Unit: unit, Value: value}
			}
		}

		rows = append(rows, row)
	}

	return rows, nil
}

// sizeCommand is the cheap "how big is this" command for a type, with the unit its
// answer is measured in. An empty unit means there is no such command.
func sizeCommand(keyType, key string) ([]interface{}, string) {
	switch keyType {
	case "string":
		return []interface{}{"STRLEN", key}, "bytes"
	case "list":
		return []interface{}{"LLEN", key}, "items"
	case "hash":
		return []interface{}{"HLEN", key}, "items"
	case "set":
		return []interface{}{"SCARD", key}, "items"
	case "zset":
		return []interface{}{"ZCARD", key}, "items"
	case "stream":
		return []interface{}{"XLEN", key}, "items"
	default:
		return nil, ""
	}
}

// parseCursor reads the opaque cursor the client sends back. Anything unparseable
// restarts the iteration rather than failing the request — a stale cursor from a
// previous page load is a normal thing to receive.
func parseCursor(raw string) uint64 {
	if raw == "" {
		return 0
	}

	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}

	return cursor
}
