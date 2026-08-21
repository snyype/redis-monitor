// Package config holds every knob the monitor reads, mirroring the PHP
// config/redis-monitor.php it was ported from: same names, same defaults, so the
// configuration table in that project's README transfers unchanged.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Redis is the server this monitor inspects.
//
// Note there is deliberately no key-prefix option. The PHP version needed a raw
// client precisely because Laravel's Redis manager prefixes every command, which
// would double-prefix the key names SCAN hands back. go-redis prefixes nothing,
// so the monitor shows exactly what is on the server — do not add a prefix here.
type Redis struct {
	Scheme       string
	Host         string
	Port         int
	Username     string
	Password     string
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

// Addr is the host:port go-redis dials.
func (r Redis) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// Config is the whole application configuration.
type Config struct {
	// Transport
	HTTPAddr string
	Dev      bool
	DataDir  string
	// TrustProxyHeaders makes the rate limiter read X-Forwarded-For. Only turn it
	// on behind a proxy that sets the header itself: a direct client can forge it
	// and hand itself a fresh bucket on every request.
	TrustProxyHeaders bool

	// Access
	//
	// Two ways in, and they are not alternatives to each other: a human logs in
	// with Username/Password and gets a session, while Token is the static
	// credential for scripts and curl. Either alone is enough to boot.
	Token      string
	Username   string
	Password   string
	SessionTTL int

	TOTPSecret           string
	RequireAuthenticator bool
	// RequireAuthenticatorOnLogin extends the authenticator from the delete path to
	// the login itself, so a leaked password alone does not get anybody in.
	RequireAuthenticatorOnLogin bool

	// Feature flag — false makes every /api/redis-monitor/* route 404.
	Enabled bool

	ConnectionName string
	Redis          Redis
	Databases      []int

	// Browsing limits
	PageSize          int
	MaxPageSize       int
	ScanCount         int
	MaxScanIterations int

	// Overview sampling
	MetricsSampleSize   int
	MetricsCacheSeconds int
	TopPrefixes         int

	// Namespace discovery
	NamespaceSampleSize   int
	NamespaceCacheSeconds int
	MaxNamespaces         int

	// Stats deep sample
	StatsSampleSize    int
	StatsCacheSeconds  int
	StatsExpiryDays    int
	StatsTopNamespaces int
	StatsLargestKeys   int
	StatsCollectMemory bool
	StatsCollectIdle   bool

	// Recorded trend
	HistoryEnabled     bool
	HistoryDays        int
	HistoryHours       int
	HistoryMinInterval int

	// Value previews
	PreviewBytes    int
	PreviewElements int

	// Delete
	AllowDelete      bool
	MaxKeysPerDelete int
	CodeReuseWindow  int

	// Redaction
	RedactKeyPatterns   []*regexp.Regexp
	RedactValuePatterns []*regexp.Regexp

	// Connected-clients page
	MaxClients          int
	ClientsCacheSeconds int

	// Rate limits, requests per minute
	ReadRateLimit   int
	DeleteRateLimit int
	// LoginRateLimit is deliberately much tighter than the read limit: it is the
	// one endpoint where guessing has a payoff.
	LoginRateLimit int
}

// Default redaction patterns, ported from config/redis-monitor.php. The PHP
// patterns carry the /i flag and use no backreferences, so they translate to RE2
// unchanged with an inline (?i).
var (
	defaultRedactKeyPatterns = []string{
		`(?i)(^|[:._-])(password|passwd|secret|token|otp|mpin|pin|cvv|card|credential|private[_-]?key)([:._-]|$)`,
	}

	defaultRedactValuePatterns = []string{
		`(?i)"?(password|passwd|api[_-]?key|secret|otp|mpin|cvv)"?\s*[:=]\s*"?[^"'\s,}\]]+`,
		`(?i)bearer\s+[A-Za-z0-9_\-\.]+`,
		`(?i)"?(access_token|refresh_token|authenticator_secret)"?\s*[:=]\s*"?[A-Za-z0-9_\-\.]{16,}`,
	}
)

// Load reads .env (if present) and then the environment.
func Load(envFile string) (*Config, error) {
	if err := LoadDotEnv(envFile); err != nil {
		return nil, fmt.Errorf("reading %s: %w", envFile, err)
	}

	maxDB := envInt("REDIS_MONITOR_MAX_DB", 15)
	if maxDB < 0 {
		maxDB = 0
	}

	databases := make([]int, 0, maxDB+1)
	for db := 0; db <= maxDB; db++ {
		databases = append(databases, db)
	}

	cfg := &Config{
		HTTPAddr:          envStr("HTTP_ADDR", ":8088"),
		Dev:               envBool("REDIS_MONITOR_DEV", false),
		DataDir:           envStr("REDIS_MONITOR_DATA_DIR", "data"),
		TrustProxyHeaders: envBool("REDIS_MONITOR_TRUST_PROXY", false),

		Token:      envStr("REDIS_MONITOR_TOKEN", ""),
		Username:   envStr("REDIS_MONITOR_USERNAME", ""),
		Password:   envStr("REDIS_MONITOR_PASSWORD", ""),
		SessionTTL: envInt("REDIS_MONITOR_SESSION_TTL", 28800),

		TOTPSecret:                  strings.ToUpper(strings.ReplaceAll(envStr("REDIS_MONITOR_TOTP_SECRET", ""), " ", "")),
		RequireAuthenticator:        envBool("REDIS_MONITOR_REQUIRE_AUTHENTICATOR", true),
		RequireAuthenticatorOnLogin: envBool("REDIS_MONITOR_REQUIRE_AUTHENTICATOR_ON_LOGIN", false),

		Enabled:        envBool("REDIS_MONITOR_ENABLED", true),
		ConnectionName: envStr("REDIS_MONITOR_CONNECTION", "default"),
		Databases:      databases,

		Redis: Redis{
			Scheme:       envStr("REDIS_SCHEME", "tcp"),
			Host:         envStr("REDIS_HOST", "127.0.0.1"),
			Port:         envInt("REDIS_PORT", 6379),
			Username:     nullableStr("REDIS_USERNAME"),
			Password:     nullableStr("REDIS_PASSWORD"),
			DialTimeout:  envSeconds("REDIS_TIMEOUT", 3),
			ReadTimeout:  envSeconds("REDIS_READ_TIMEOUT", 10),
			WriteTimeout: envSeconds("REDIS_WRITE_TIMEOUT", 10),
			PoolSize:     envInt("REDIS_POOL_SIZE", 10),
		},

		PageSize:          envInt("REDIS_MONITOR_PAGE_SIZE", 50),
		MaxPageSize:       envInt("REDIS_MONITOR_MAX_PAGE_SIZE", 200),
		ScanCount:         envInt("REDIS_MONITOR_SCAN_COUNT", 500),
		MaxScanIterations: envInt("REDIS_MONITOR_MAX_SCAN_ITERATIONS", 200),

		MetricsSampleSize:   envInt("REDIS_MONITOR_SAMPLE_SIZE", 2000),
		MetricsCacheSeconds: envInt("REDIS_MONITOR_METRICS_CACHE", 15),
		TopPrefixes:         envInt("REDIS_MONITOR_TOP_PREFIXES", 10),

		NamespaceSampleSize:   envInt("REDIS_MONITOR_NAMESPACE_SAMPLE_SIZE", 20000),
		NamespaceCacheSeconds: envInt("REDIS_MONITOR_NAMESPACE_CACHE", 60),
		MaxNamespaces:         envInt("REDIS_MONITOR_MAX_NAMESPACES", 200),

		StatsSampleSize:    envInt("REDIS_MONITOR_STATS_SAMPLE_SIZE", 3000),
		StatsCacheSeconds:  envInt("REDIS_MONITOR_STATS_CACHE", 60),
		StatsExpiryDays:    envInt("REDIS_MONITOR_STATS_EXPIRY_DAYS", 14),
		StatsTopNamespaces: envInt("REDIS_MONITOR_STATS_TOP_NAMESPACES", 12),
		StatsLargestKeys:   envInt("REDIS_MONITOR_STATS_LARGEST_KEYS", 10),
		StatsCollectMemory: envBool("REDIS_MONITOR_STATS_MEMORY", true),
		StatsCollectIdle:   envBool("REDIS_MONITOR_STATS_IDLE", true),

		HistoryEnabled:     envBool("REDIS_MONITOR_HISTORY_ENABLED", true),
		HistoryDays:        envInt("REDIS_MONITOR_HISTORY_DAYS", 30),
		HistoryHours:       envInt("REDIS_MONITOR_HISTORY_HOURS", 48),
		HistoryMinInterval: envInt("REDIS_MONITOR_HISTORY_MIN_INTERVAL", 120),

		PreviewBytes:    envInt("REDIS_MONITOR_PREVIEW_BYTES", 8192),
		PreviewElements: envInt("REDIS_MONITOR_PREVIEW_ELEMENTS", 100),

		AllowDelete:      envBool("REDIS_MONITOR_ALLOW_DELETE", true),
		MaxKeysPerDelete: envInt("REDIS_MONITOR_MAX_DELETE", 200),
		CodeReuseWindow:  envInt("REDIS_MONITOR_CODE_REUSE_WINDOW", 120),

		ReadRateLimit:   envInt("REDIS_MONITOR_READ_RATE_LIMIT", 120),
		DeleteRateLimit: envInt("REDIS_MONITOR_DELETE_RATE_LIMIT", 20),
		LoginRateLimit:  envInt("REDIS_MONITOR_LOGIN_RATE_LIMIT", 10),

		MaxClients:          envInt("REDIS_MONITOR_MAX_CLIENTS", 500),
		ClientsCacheSeconds: envInt("REDIS_MONITOR_CLIENTS_CACHE", 5),
	}

	var err error

	if cfg.RedactKeyPatterns, err = compilePatterns("REDIS_MONITOR_REDACT_KEY_PATTERNS", defaultRedactKeyPatterns); err != nil {
		return nil, err
	}

	if cfg.RedactValuePatterns, err = compilePatterns("REDIS_MONITOR_REDACT_VALUE_PATTERNS", defaultRedactValuePatterns); err != nil {
		return nil, err
	}

	cfg.clamp()

	return cfg, cfg.Validate()
}

// clamp applies the same max()/min() floors the PHP service applies at every read
// site, once, here — so the rest of the code can trust the numbers.
func (c *Config) clamp() {
	c.PageSize = atLeast(c.PageSize, 1)
	c.MaxPageSize = atLeast(c.MaxPageSize, c.PageSize)
	c.ScanCount = atLeast(c.ScanCount, 10)
	c.MaxScanIterations = atLeast(c.MaxScanIterations, 1)

	c.MetricsSampleSize = atLeast(c.MetricsSampleSize, 0)
	c.MetricsCacheSeconds = atLeast(c.MetricsCacheSeconds, 0)
	c.TopPrefixes = atLeast(c.TopPrefixes, 1)

	c.NamespaceSampleSize = atLeast(c.NamespaceSampleSize, 0)
	c.NamespaceCacheSeconds = atLeast(c.NamespaceCacheSeconds, 0)
	c.MaxNamespaces = atLeast(c.MaxNamespaces, 1)

	c.StatsSampleSize = atLeast(c.StatsSampleSize, 0)
	c.StatsCacheSeconds = atLeast(c.StatsCacheSeconds, 0)
	c.StatsExpiryDays = atLeast(c.StatsExpiryDays, 1)
	c.StatsTopNamespaces = atLeast(c.StatsTopNamespaces, 1)
	c.StatsLargestKeys = atLeast(c.StatsLargestKeys, 1)

	c.HistoryDays = atLeast(c.HistoryDays, 1)
	c.HistoryHours = atLeast(c.HistoryHours, 1)
	c.HistoryMinInterval = atLeast(c.HistoryMinInterval, 0)

	c.PreviewBytes = atLeast(c.PreviewBytes, 64)
	c.PreviewElements = atLeast(c.PreviewElements, 1)

	c.MaxKeysPerDelete = atLeast(c.MaxKeysPerDelete, 1)
	c.CodeReuseWindow = atLeast(c.CodeReuseWindow, 1)

	c.ReadRateLimit = atLeast(c.ReadRateLimit, 1)
	c.DeleteRateLimit = atLeast(c.DeleteRateLimit, 1)
	c.LoginRateLimit = atLeast(c.LoginRateLimit, 1)

	c.MaxClients = atLeast(c.MaxClients, 1)
	c.ClientsCacheSeconds = atLeast(c.ClientsCacheSeconds, 0)
	c.SessionTTL = atLeast(c.SessionTTL, 60)

	c.Redis.Port = atLeast(c.Redis.Port, 1)
	c.Redis.PoolSize = atLeast(c.Redis.PoolSize, 1)
}

// Validate refuses to boot on a configuration that would be unsafe rather than
// merely wrong — chiefly an unauthenticated monitor on a real Redis.
func (c *Config) Validate() error {
	if (c.Username == "") != (c.Password == "") {
		return errors.New("REDIS_MONITOR_USERNAME and REDIS_MONITOR_PASSWORD must be set together, or neither")
	}

	if c.Token == "" && !c.CanLogin() && !c.Dev {
		return errors.New("no way in is configured: set REDIS_MONITOR_TOKEN, or REDIS_MONITOR_USERNAME and REDIS_MONITOR_PASSWORD, or REDIS_MONITOR_DEV=true to run without either")
	}

	// A login form that can never be satisfied is worse than no login form: the
	// operator would sit in front of a code box with nothing to type.
	if c.CanLogin() && c.RequireAuthenticatorOnLogin && c.TOTPSecret == "" {
		return errors.New("REDIS_MONITOR_REQUIRE_AUTHENTICATOR_ON_LOGIN is set but REDIS_MONITOR_TOTP_SECRET is empty")
	}

	if c.Redis.Host == "" {
		return errors.New("REDIS_HOST is empty")
	}

	if len(c.Databases) == 0 {
		return errors.New("no selectable databases: REDIS_MONITOR_MAX_DB is out of range")
	}

	return nil
}

// DefaultDatabase is the database used when a request names none.
func (c *Config) DefaultDatabase() int {
	if len(c.Databases) == 0 {
		return 0
	}

	return c.Databases[0]
}

// AllowsDatabase reports whether the UI may switch to this database index.
func (c *Config) AllowsDatabase(db int) bool {
	for _, allowed := range c.Databases {
		if allowed == db {
			return true
		}
	}

	return false
}

// CanDelete reports whether deletes are possible at all. Requiring an
// authenticator code with no secret configured is a misconfiguration, not a
// licence to delete without one — so it reads as "cannot delete".
// CanLogin reports whether username/password login is configured at all. When it
// is not, the page falls back to asking for the static bearer token.
func (c *Config) CanLogin() bool {
	return c.Username != "" && c.Password != ""
}

// LoginNeedsCode reports whether a login must carry an authenticator code.
func (c *Config) LoginNeedsCode() bool {
	return c.CanLogin() && c.RequireAuthenticatorOnLogin && c.TOTPSecret != ""
}

func (c *Config) CanDelete() (bool, string) {
	if !c.AllowDelete {
		return false, "Deleting keys is disabled on this environment."
	}

	if c.RequireAuthenticator && c.TOTPSecret == "" {
		return false, "Deleting keys needs an authenticator code, but REDIS_MONITOR_TOTP_SECRET is not configured."
	}

	return true, ""
}

func compilePatterns(envName string, defaults []string) ([]*regexp.Regexp, error) {
	sources := defaults

	// A newline-separated override, so an operator can add a pattern without a
	// rebuild. An empty string is not "use the defaults" — it is "redact nothing".
	if raw, ok := os.LookupEnv(envName); ok {
		sources = nil

		for _, line := range strings.Split(raw, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				sources = append(sources, line)
			}
		}
	}

	compiled := make([]*regexp.Regexp, 0, len(sources))

	for _, source := range sources {
		pattern, err := regexp.Compile(source)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot compile %q: %w", envName, source, err)
		}

		compiled = append(compiled, pattern)
	}

	return compiled, nil
}

func envStr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}

	return fallback
}

// nullableStr treats the literal "null" as absent, the way Laravel's config cast
// does — REDIS_PASSWORD=null means "no password", not a password of "null".
func nullableStr(name string) string {
	value := strings.TrimSpace(os.Getenv(name))

	if value == "" || strings.EqualFold(value, "null") || strings.EqualFold(value, "(null)") {
		return ""
	}

	return value
}

func envInt(name string, fallback int) int {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}

	return parsed
}

func envBool(name string, fallback bool) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off", "":
		return false
	default:
		return fallback
	}
}

func envSeconds(name string, fallback float64) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return time.Duration(fallback * float64(time.Second))
	}

	seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || seconds <= 0 {
		return time.Duration(fallback * float64(time.Second))
	}

	return time.Duration(seconds * float64(time.Second))
}

func atLeast(value, floor int) int {
	if value < floor {
		return floor
	}

	return value
}
