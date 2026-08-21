// Command seed writes a small, predictable set of keys so the monitor can be
// exercised end to end: one key of every Redis type, a JSON payload, a key that
// expires shortly, and a key whose name must trigger redaction.
//
// Every key it writes is namespaced under a prefix you can delete in one go, so a
// smoke test never leaves litter behind:
//
//	go run ./cmd/seed              # write the fixtures
//	go run ./cmd/seed -clean       # remove them again
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"redismonitor/internal/config"
)

const prefix = "monitorseed:"

func main() {
	envFile := flag.String("env", ".env", "path to the .env file")
	db := flag.Int("db", 0, "database index to seed")
	clean := flag.Bool("clean", false, "delete the seeded keys instead of writing them")
	flag.Parse()

	if err := run(*envFile, *db, *clean); err != nil {
		log.Fatal(err)
	}
}

func run(envFile string, db int, clean bool) error {
	// The seeder only needs the Redis half of the configuration, so it tolerates a
	// missing bearer token that config.Load would otherwise reject.
	if err := os.Setenv("REDIS_MONITOR_DEV", "true"); err != nil {
		return err
	}

	cfg, err := config.Load(envFile)
	if err != nil {
		return err
	}

	client := redis.NewClient(&redis.Options{
		Addr:        cfg.Redis.Addr(),
		Username:    cfg.Redis.Username,
		Password:    cfg.Redis.Password,
		DB:          db,
		DialTimeout: cfg.Redis.DialTimeout,
	})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cannot reach redis at %s: %w", cfg.Redis.Addr(), err)
	}

	if clean {
		return remove(ctx, client)
	}

	return write(ctx, client)
}

func write(ctx context.Context, client *redis.Client) error {
	// A string, and a JSON string the drawer should pretty-print.
	if err := client.Set(ctx, prefix+"string:greeting", "hello from the seeder", 0).Err(); err != nil {
		return err
	}

	if err := client.Set(ctx, prefix+"json:branch",
		`{"id":42,"name":"Head Office","open":true,"tags":["main","kyc"],"limits":{"daily":250000}}`, 0).Err(); err != nil {
		return err
	}

	// A key that expires soon, so the "expiring" highlight and the hourly forecast
	// have something to show.
	if err := client.Set(ctx, prefix+"cache:short-lived", "gone in four minutes", 4*time.Minute).Err(); err != nil {
		return err
	}

	// A key whose NAME matches the redaction patterns: its value must never come
	// back from /key.
	if err := client.Set(ctx, prefix+"user:token:abc", "should-never-be-returned", time.Hour).Err(); err != nil {
		return err
	}

	// A value carrying a secret inside otherwise harmless JSON, which value-level
	// redaction has to mask.
	if err := client.Set(ctx, prefix+"session:payload",
		`{"user":"aashish","access_token":"eyJhbGciOiJIUzI1NiwidHlwIjoiSldUIn0","remember":true}`, time.Hour).Err(); err != nil {
		return err
	}

	if err := client.Del(ctx, prefix+"list:queue").Err(); err != nil {
		return err
	}

	if err := client.RPush(ctx, prefix+"list:queue", "first", "second", "third").Err(); err != nil {
		return err
	}

	if err := client.SAdd(ctx, prefix+"set:branches", "kathmandu", "pokhara", "biratnagar").Err(); err != nil {
		return err
	}

	if err := client.ZAdd(ctx, prefix+"zset:leaderboard",
		redis.Z{Score: 12, Member: "alpha"},
		redis.Z{Score: 7.5, Member: "beta"},
		redis.Z{Score: 30, Member: "gamma"},
	).Err(); err != nil {
		return err
	}

	if err := client.HSet(ctx, prefix+"hash:profile",
		"name", "Head Office",
		"district", "Kathmandu",
		// A field whose NAME is the sensitive part — hash-pair redaction has to mask
		// the value even though the value itself looks innocent.
		"password", "plaintext-should-be-masked",
	).Err(); err != nil {
		return err
	}

	if err := client.Del(ctx, prefix+"stream:events").Err(); err != nil {
		return err
	}

	for index := 0; index < 3; index++ {
		if err := client.XAdd(ctx, &redis.XAddArgs{
			Stream: prefix + "stream:events",
			Values: map[string]interface{}{"event": "seeded", "index": strconv.Itoa(index)},
		}).Err(); err != nil {
			return err
		}
	}

	// A second namespace, so the namespace dropdown and the breakdown table have
	// more than one row to rank.
	for index := 0; index < 25; index++ {
		key := fmt.Sprintf("%sbulk|item-%02d", prefix, index)

		if err := client.Set(ctx, key, fmt.Sprintf("filler value %02d", index), time.Duration(index+1)*time.Hour).Err(); err != nil {
			return err
		}
	}

	fmt.Println("seeded fixtures under " + prefix)

	return nil
}

func remove(ctx context.Context, client *redis.Client) error {
	var (
		cursor  uint64
		removed int64
	)

	for {
		keys, next, err := client.Scan(ctx, cursor, globEscape(prefix)+"*", 500).Result()
		if err != nil {
			return err
		}

		if len(keys) > 0 {
			count, err := client.Del(ctx, keys...).Result()
			if err != nil {
				return err
			}

			removed += count
		}

		cursor = next

		if cursor == 0 {
			break
		}
	}

	fmt.Printf("removed %d seeded keys\n", removed)

	return nil
}

// globEscape keeps the seed prefix a literal in the MATCH pattern, so -clean can
// never widen into something it should not touch.
func globEscape(literal string) string {
	escaped := make([]byte, 0, len(literal)+4)

	for index := 0; index < len(literal); index++ {
		switch literal[index] {
		case '\\', '*', '?', '[', ']', '^':
			escaped = append(escaped, '\\')
		}

		escaped = append(escaped, literal[index])
	}

	return string(escaped)
}
