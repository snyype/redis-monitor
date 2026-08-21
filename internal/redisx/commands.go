package redisx

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// Doer is the raw-command escape hatch shared by a client and a pipeline, so the
// same helper works inline and inside a pipelined batch.
type Doer interface {
	Do(ctx context.Context, args ...interface{}) *redis.Cmd
}

// Commands a server is allowed to refuse.
//
// MEMORY USAGE needs Redis 4 and is disabled outright on some managed offerings;
// OBJECT ENCODING is occasionally renamed; OBJECT IDLETIME errors under an LFU
// eviction policy, which is a configuration choice rather than a fault. None of
// them are worth failing a request over — the chart that needed one says why it is
// empty instead.

// OptionalInt runs a command that may be refused and reports whether it answered.
func OptionalInt(ctx context.Context, doer Doer, args ...interface{}) (int64, bool) {
	return ToInt64(reply(ctx, doer, args...))
}

// OptionalString runs a command that may be refused and reports whether it
// answered.
func OptionalString(ctx context.Context, doer Doer, args ...interface{}) (string, bool) {
	value := reply(ctx, doer, args...)
	if value == nil {
		return "", false
	}

	return ToString(value), true
}

// MemoryUsage is MEMORY USAGE with SAMPLES 0 — an exact figure rather than a
// sampled estimate, which matters for a "largest keys" table.
func MemoryUsage(ctx context.Context, doer Doer, key string) (int64, bool) {
	return OptionalInt(ctx, doer, "MEMORY", "USAGE", key, "SAMPLES", 0)
}

// ObjectIdleTime is OBJECT IDLETIME in seconds.
func ObjectIdleTime(ctx context.Context, doer Doer, key string) (int64, bool) {
	return OptionalInt(ctx, doer, "OBJECT", "IDLETIME", key)
}

// ObjectEncoding is OBJECT ENCODING.
func ObjectEncoding(ctx context.Context, doer Doer, key string) (string, bool) {
	return OptionalString(ctx, doer, "OBJECT", "ENCODING", key)
}

func reply(ctx context.Context, doer Doer, args ...interface{}) interface{} {
	value, err := doer.Do(ctx, args...).Result()
	if err != nil {
		return nil
	}

	return value
}

// ToInt64 coerces a raw reply to an integer. Redis answers integers as int64 over
// RESP2 and occasionally as a bulk string, so both are accepted.
func ToInt64(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, false
		}

		return parsed, true
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}

		return parsed, true
	default:
		return 0, false
	}
}

// ToString coerces a raw reply to a string.
func ToString(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "1"
		}

		return "0"
	default:
		return ""
	}
}

// CmdInt64 reads a pipelined reply as an integer, reporting refusal as false so a
// dropped command never masquerades as a zero.
func CmdInt64(cmd *redis.Cmd) (int64, bool) {
	if cmd == nil || cmd.Err() != nil {
		return 0, false
	}

	value, err := cmd.Result()
	if err != nil {
		return 0, false
	}

	return ToInt64(value)
}

// CmdString reads a pipelined reply as a string.
func CmdString(cmd *redis.Cmd) (string, bool) {
	if cmd == nil || cmd.Err() != nil {
		return "", false
	}

	value, err := cmd.Result()
	if err != nil {
		return "", false
	}

	return ToString(value), true
}
