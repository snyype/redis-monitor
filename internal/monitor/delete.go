package monitor

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// DeleteResult reports what happened to each named key.
type DeleteResult struct {
	Deleted  int         `json:"deleted"`
	Missing  int         `json:"missing"`
	Database int         `json:"database"`
	Results  []DeleteRow `json:"results"`
}

type DeleteRow struct {
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
}

// DeleteKeys removes an explicit list of keys.
//
// There is deliberately no pattern delete. The caller has to have seen and named
// every key it is removing, so a stray "*" can never wipe a database from this
// screen — and no amount of UI convenience is worth reintroducing that.
//
// UNLINK is preferred because it frees the memory on a background thread; DEL is
// the fallback for Redis before 4.0, which has no UNLINK at all.
func (s *Service) DeleteKeys(ctx context.Context, db int, keys []string) (*DeleteResult, error) {
	client, err := s.client(db)
	if err != nil {
		return nil, err
	}

	unique := dedupe(keys)

	result := &DeleteResult{
		Database: db,
		Results:  make([]DeleteRow, 0, len(unique)),
	}

	if len(unique) == 0 {
		return result, nil
	}

	removed, err := unlinkOrDelete(ctx, client, unique)
	if err != nil {
		return nil, err
	}

	for index, key := range unique {
		deleted := removed[index] > 0

		if deleted {
			result.Deleted++
		} else {
			result.Missing++
		}

		result.Results = append(result.Results, DeleteRow{Key: key, Deleted: deleted})
	}

	// Drop the cached payloads for this database so the charts and the namespace
	// dropdown stop showing keys that are gone.
	s.cache.Forget(cacheKey("overview", db))
	s.cache.Forget(cacheKey("namespaces", db))
	s.cache.Forget(cacheKey("stats", db))

	return result, nil
}

// unlinkOrDelete removes each key individually so the response can say which ones
// were actually there, and falls back to DEL when the server has no UNLINK.
func unlinkOrDelete(ctx context.Context, client *redis.Client, keys []string) ([]int64, error) {
	removed, err := runRemoval(ctx, client, "UNLINK", keys)
	if err == nil {
		return removed, nil
	}

	if !isReplyError(err) {
		return nil, err
	}

	return runRemoval(ctx, client, "DEL", keys)
}

func runRemoval(ctx context.Context, client *redis.Client, command string, keys []string) ([]int64, error) {
	cmds := make([]*redis.Cmd, len(keys))

	_, err := client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, key := range keys {
			cmds[index] = pipe.Do(ctx, command, key)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	removed := make([]int64, len(keys))

	for index := range keys {
		if cmds[index] == nil {
			continue
		}

		if cmdErr := cmds[index].Err(); cmdErr != nil {
			return nil, cmdErr
		}

		value, _ := cmds[index].Int64()
		removed[index] = value
	}

	return removed, nil
}

// dedupe keeps the caller's order but removes repeats, so a double-checked row in
// the UI cannot inflate the deleted count.
func dedupe(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	unique := make([]string, 0, len(keys))

	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}

		seen[key] = true
		unique = append(unique, key)
	}

	return unique
}
