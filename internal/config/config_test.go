package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing env file: %v", err)
	}

	return path
}

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		line  string
		name  string
		value string
		ok    bool
	}{
		{"FOO=bar", "FOO", "bar", true},
		{"  FOO = bar  ", "FOO", "bar", true},
		{"export FOO=bar", "FOO", "bar", true},
		{`FOO="bar baz"`, "FOO", "bar baz", true},
		{`FOO='bar baz'`, "FOO", "bar baz", true},
		{`FOO="line\nbreak"`, "FOO", "line\nbreak", true},
		// An unquoted trailing comment is not part of the value.
		{"FOO=bar # a comment", "FOO", "bar", true},
		// A quoted value keeps its hash.
		{`FOO="bar # kept"`, "FOO", "bar # kept", true},
		{"FOO=", "FOO", "", true},
		{"# comment", "", "", false},
		{"", "", "", false},
		{"no equals sign", "", "", false},
		{"=novalue", "", "", false},
	}

	for _, testCase := range cases {
		name, value, ok := parseDotEnvLine(testCase.line)

		if ok != testCase.ok || name != testCase.name || value != testCase.value {
			t.Errorf("parseDotEnvLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				testCase.line, name, value, ok, testCase.name, testCase.value, testCase.ok)
		}
	}
}

func TestRealEnvironmentBeatsTheFile(t *testing.T) {
	path := writeEnv(t, "REDIS_MONITOR_TOKEN=from-file\nREDIS_HOST=file-host\n")

	t.Setenv("REDIS_MONITOR_TOKEN", "from-environment")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Token != "from-environment" {
		t.Errorf("token = %q, want the environment to win", cfg.Token)
	}

	if cfg.Redis.Host != "file-host" {
		t.Errorf("host = %q, want the file value where the environment is silent", cfg.Redis.Host)
	}
}

func TestMissingEnvFileIsFine(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")

	if _, err := Load(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Errorf("a missing .env must not be an error: %v", err)
	}
}

func TestRefusesToBootWithoutAToken(t *testing.T) {
	path := writeEnv(t, "REDIS_HOST=127.0.0.1\n")

	if _, err := Load(path); err == nil {
		t.Fatal("expected Load to refuse an empty token outside dev mode")
	}

	// Dev mode is the only way to run unauthenticated, and it has to be explicit.
	t.Setenv("REDIS_MONITOR_DEV", "true")

	if _, err := Load(path); err != nil {
		t.Errorf("dev mode should allow an empty token: %v", err)
	}
}

func TestNullPasswordMeansNoPassword(t *testing.T) {
	// Plenty of tooling writes REDIS_PASSWORD=null to mean "no password"; taken
	// literally the monitor would try to authenticate with the word "null" and fail
	// to connect at all.
	path := writeEnv(t, "REDIS_MONITOR_TOKEN=t\nREDIS_PASSWORD=null\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Redis.Password != "" {
		t.Errorf("password = %q, want empty", cfg.Redis.Password)
	}
}

func TestShippedDefaults(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"PageSize", cfg.PageSize, 50},
		{"MaxPageSize", cfg.MaxPageSize, 200},
		{"ScanCount", cfg.ScanCount, 500},
		{"MaxScanIterations", cfg.MaxScanIterations, 200},
		{"MetricsSampleSize", cfg.MetricsSampleSize, 2000},
		{"MetricsCacheSeconds", cfg.MetricsCacheSeconds, 15},
		{"TopPrefixes", cfg.TopPrefixes, 10},
		{"NamespaceSampleSize", cfg.NamespaceSampleSize, 20000},
		{"MaxNamespaces", cfg.MaxNamespaces, 200},
		{"StatsSampleSize", cfg.StatsSampleSize, 3000},
		{"StatsExpiryDays", cfg.StatsExpiryDays, 14},
		{"HistoryDays", cfg.HistoryDays, 30},
		{"HistoryHours", cfg.HistoryHours, 48},
		{"PreviewBytes", cfg.PreviewBytes, 8192},
		{"PreviewElements", cfg.PreviewElements, 100},
		{"MaxKeysPerDelete", cfg.MaxKeysPerDelete, 200},
		{"CodeReuseWindow", cfg.CodeReuseWindow, 120},
	}

	for _, testCase := range cases {
		if testCase.got != testCase.want {
			t.Errorf("%s = %d, want %d", testCase.name, testCase.got, testCase.want)
		}
	}

	if len(cfg.Databases) != 16 {
		t.Errorf("databases = %d, want 16 (0-15)", len(cfg.Databases))
	}

	if !cfg.Enabled || !cfg.AllowDelete || !cfg.RequireAuthenticator || !cfg.HistoryEnabled {
		t.Error("the shipped defaults should be enabled, deletable, authenticator-gated and recording")
	}
}

func TestClampFloors(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")
	t.Setenv("REDIS_MONITOR_PAGE_SIZE", "0")
	t.Setenv("REDIS_MONITOR_MAX_PAGE_SIZE", "1")
	t.Setenv("REDIS_MONITOR_SCAN_COUNT", "1")
	t.Setenv("REDIS_MONITOR_MAX_SCAN_ITERATIONS", "0")
	t.Setenv("REDIS_MONITOR_PREVIEW_BYTES", "1")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PageSize != 1 {
		t.Errorf("PageSize = %d, want a floor of 1", cfg.PageSize)
	}

	if cfg.ScanCount != 10 {
		t.Errorf("ScanCount = %d, want a floor of 10", cfg.ScanCount)
	}

	if cfg.MaxScanIterations != 1 {
		t.Errorf("MaxScanIterations = %d, want a floor of 1 — zero would scan nothing", cfg.MaxScanIterations)
	}

	if cfg.PreviewBytes != 64 {
		t.Errorf("PreviewBytes = %d, want a floor of 64", cfg.PreviewBytes)
	}

	if cfg.MaxPageSize < cfg.PageSize {
		t.Errorf("MaxPageSize (%d) must never be below PageSize (%d)", cfg.MaxPageSize, cfg.PageSize)
	}
}

func TestCanDeleteRefusesWhenACodeIsRequiredButNoSecretIsSet(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A misconfiguration must read as "cannot delete", never as "delete without a
	// code".
	if ok, reason := cfg.CanDelete(); ok || reason == "" {
		t.Errorf("CanDelete = (%v, %q), want a refusal explaining the missing secret", ok, reason)
	}

	cfg.TOTPSecret = "GEZDGNBVGY3TQOJQ"

	if ok, _ := cfg.CanDelete(); !ok {
		t.Error("CanDelete should allow deletes once a secret is configured")
	}

	cfg.AllowDelete = false

	if ok, _ := cfg.CanDelete(); ok {
		t.Error("CanDelete must respect REDIS_MONITOR_ALLOW_DELETE=false")
	}
}

func TestAllowsDatabase(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")
	t.Setenv("REDIS_MONITOR_MAX_DB", "3")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.AllowsDatabase(0) || !cfg.AllowsDatabase(3) {
		t.Error("0 and 3 should both be selectable")
	}

	if cfg.AllowsDatabase(4) || cfg.AllowsDatabase(-1) {
		t.Error("a database outside the configured range must be refused")
	}

	if cfg.DefaultDatabase() != 0 {
		t.Errorf("DefaultDatabase = %d, want 0", cfg.DefaultDatabase())
	}
}

func TestBadRedactionPatternIsARefusalNotASilentDrop(t *testing.T) {
	t.Setenv("REDIS_MONITOR_TOKEN", "t")
	t.Setenv("REDIS_MONITOR_REDACT_KEY_PATTERNS", "([unclosed")

	if _, err := Load(filepath.Join(t.TempDir(), "absent.env")); err == nil {
		t.Fatal("expected an uncompilable redaction pattern to fail the boot")
	}
}
