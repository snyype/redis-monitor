package monitor

import (
	"strings"
	"testing"

	"redismonitor/internal/config"
)

// defaultRedactor builds a redactor from the shipped patterns, so these tests
// exercise the real configuration rather than a convenient stand-in.
func defaultRedactor(t *testing.T) redactor {
	t.Helper()

	t.Setenv("REDIS_MONITOR_DEV", "true")

	cfg, err := config.Load("testdata/does-not-exist.env")
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	return newRedactor(cfg.RedactKeyPatterns, cfg.RedactValuePatterns)
}

func TestIsSensitiveKey(t *testing.T) {
	redact := defaultRedactor(t)

	sensitive := []string{
		"user:token:abc",
		"auth.password.reset",
		"otp:9812345678",
		"session_secret_store",
		"customer:mpin:42",
		"x:private-key:1",
		"card:1234",
		"PASSWORD:reset",
		"credential",
	}

	for _, key := range sensitive {
		if !redact.IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", key)
		}
	}

	harmless := []string{
		"laravel_cache:branches",
		"queue|default",
		// "tokens" is not "token": the pattern anchors on separators so an ordinary
		// word containing the fragment is not swept up.
		"tokenizer:config",
		"user:profile:42",
	}

	for _, key := range harmless {
		if redact.IsSensitiveKey(key) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", key)
		}
	}
}

func TestValueRedaction(t *testing.T) {
	redact := defaultRedactor(t)

	cases := []struct {
		name  string
		value string
		// leaked must not appear anywhere in the masked output.
		leaked string
	}{
		{"json password", `{"user":"a","password":"hunter2"}`, "hunter2"},
		{"api key", `api_key=abcdef123456`, "abcdef123456"},
		{"bearer token", `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9`, "eyJhbGciOiJIUzI1NiJ9"},
		{"access token", `{"access_token":"0123456789abcdefghij"}`, "0123456789abcdefghij"},
		{"otp", `otp: 483920`, "483920"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			masked := redact.Value(testCase.value)

			if strings.Contains(masked, testCase.leaked) {
				t.Errorf("value %q was not masked: got %q", testCase.value, masked)
			}

			if !strings.Contains(masked, redactedPlaceholder) {
				t.Errorf("expected %s in %q", redactedPlaceholder, masked)
			}
		})
	}
}

func TestValueRedactionLeavesHarmlessValuesAlone(t *testing.T) {
	redact := defaultRedactor(t)

	value := `{"id":42,"name":"Head Office","open":true}`

	if got := redact.Value(value); got != value {
		t.Errorf("harmless value was altered:\n got %q\nwant %q", got, value)
	}
}

func TestPairMasksValueWhenTheFieldNameIsTheSensitivePart(t *testing.T) {
	redact := defaultRedactor(t)

	// The value patterns only fire on "password=x" style text. In a hash the field
	// and the value arrive separately, so a field called "password" would otherwise
	// hand its value over untouched.
	pair := redact.Pair("password", "plaintext")

	if pair.Value != redactedPlaceholder {
		t.Errorf("Pair(password, plaintext).Value = %q, want %s", pair.Value, redactedPlaceholder)
	}

	if pair.Field != "password" {
		t.Errorf("Pair field = %q, want the field name to survive", pair.Field)
	}
}

func TestPairLeavesOrdinaryFieldsAlone(t *testing.T) {
	redact := defaultRedactor(t)

	pair := redact.Pair("district", "Kathmandu")

	if pair.Field != "district" || pair.Value != "Kathmandu" {
		t.Errorf("Pair(district, Kathmandu) = %+v, want it untouched", pair)
	}
}

func TestRedactionCanBeTurnedOff(t *testing.T) {
	// An explicitly empty override means "redact nothing", not "use the defaults" —
	// worth pinning, because the opposite reading would silently expose values.
	t.Setenv("REDIS_MONITOR_DEV", "true")
	t.Setenv("REDIS_MONITOR_REDACT_KEY_PATTERNS", "")
	t.Setenv("REDIS_MONITOR_REDACT_VALUE_PATTERNS", "")

	cfg, err := config.Load("testdata/does-not-exist.env")
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	redact := newRedactor(cfg.RedactKeyPatterns, cfg.RedactValuePatterns)

	if redact.IsSensitiveKey("user:token:abc") {
		t.Error("expected no key redaction when the pattern list is explicitly empty")
	}

	if got := redact.Value(`password=hunter2`); got != `password=hunter2` {
		t.Errorf("expected no value redaction, got %q", got)
	}
}
