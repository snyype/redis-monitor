package totp

import (
	"testing"
	"time"
)

// rfc6238Secret is the 20-byte ASCII secret "12345678901234567890" from RFC 6238
// appendix B, base32-encoded — the standard's own SHA1 test vector.
const rfc6238Secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestVerifyAgainstRFC6238Vectors(t *testing.T) {
	// RFC 6238 appendix B, the SHA1 rows. The published values are 8 digits; the
	// last six are what a 6-digit authenticator shows.
	cases := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}

	for _, testCase := range cases {
		at := time.Unix(testCase.unix, 0)

		if !Verify(rfc6238Secret, testCase.code, 0, at) {
			t.Errorf("Verify(%s) at %d = false, want true", testCase.code, testCase.unix)
		}
	}
}

func TestVerifyRejectsMalformedCodes(t *testing.T) {
	at := time.Unix(59, 0)

	for _, code := range []string{"", "12345", "1234567", "abcdef", "12 345", "-12345"} {
		if Verify(rfc6238Secret, code, 1, at) {
			t.Errorf("Verify(%q) = true, want false", code)
		}
	}
}

func TestVerifyTrimsSurroundingSpace(t *testing.T) {
	// Authenticator apps display codes as "287 082"; a pasted code often carries
	// surrounding whitespace, which should not be the user's problem.
	if !Verify(rfc6238Secret, "  287082  ", 0, time.Unix(59, 0)) {
		t.Error("a code with surrounding whitespace was rejected")
	}
}

func TestVerifyDriftWindow(t *testing.T) {
	// 287082 is the code for the step containing t=59.
	previousStep := time.Unix(59+30, 0)

	if Verify(rfc6238Secret, "287082", 0, previousStep) {
		t.Error("window=0 accepted a code from the previous step")
	}

	if !Verify(rfc6238Secret, "287082", 1, previousStep) {
		t.Error("window=1 rejected a code one step old; ±30s drift must be tolerated")
	}

	if Verify(rfc6238Secret, "287082", 1, time.Unix(59+90, 0)) {
		t.Error("window=1 accepted a code three steps old")
	}
}

func TestVerifyRejectsBadSecret(t *testing.T) {
	if Verify("", "287082", 1, time.Unix(59, 0)) {
		t.Error("an empty secret must never verify")
	}

	if Verify("not!valid!base32", "287082", 1, time.Unix(59, 0)) {
		t.Error("an undecodable secret must never verify")
	}
}

func TestVerifyAcceptsSecretsAsUsersPasteThem(t *testing.T) {
	// Lowercase, spaced and padded forms all come from real authenticator UIs.
	for _, secret := range []string{
		"gezdgnbvgy3tqojqgezdgnbvgy3tqojq",
		"GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ",
		"GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ====",
	} {
		if !Verify(secret, "287082", 0, time.Unix(59, 0)) {
			t.Errorf("secret form %q was rejected", secret)
		}
	}
}

func TestGenerateSecretRoundTrips(t *testing.T) {
	secret, err := GenerateSecret(20)
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	if len(secret) != 32 {
		t.Errorf("secret length = %d, want 32 base32 characters for 20 bytes", len(secret))
	}

	key, err := decodeSecret(secret)
	if err != nil {
		t.Fatalf("generated secret does not decode: %v", err)
	}

	if len(key) != 20 {
		t.Errorf("decoded key = %d bytes, want 20", len(key))
	}

	// A freshly generated secret must verify its own current code.
	now := time.Now()
	code := hotp(key, now.Unix()/int64(Period.Seconds()))

	if !Verify(secret, code, 0, now) {
		t.Error("a generated secret did not verify its own code")
	}
}

func TestProvisioningURI(t *testing.T) {
	uri := ProvisioningURI("ABCDEF", "ops@example.com", "Redis Monitor")

	for _, want := range []string{
		"otpauth://totp/",
		"secret=ABCDEF",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !contains(uri, want) {
			t.Errorf("provisioning URI %q is missing %q", uri, want)
		}
	}
}

func TestSpentCodesBurnsACodeForTheWindow(t *testing.T) {
	spent := NewSpentCodes(120 * time.Second)
	at := time.Unix(1_000_000, 0)

	if spent.IsSpent("287082", at) {
		t.Fatal("a fresh code reported as spent")
	}

	spent.Burn("287082", at)

	if !spent.IsSpent("287082", at) {
		t.Error("a burned code must not authorise a second batch")
	}

	// A different code is unaffected.
	if spent.IsSpent("999999", at) {
		t.Error("burning one code must not blacklist another")
	}

	// Past the reuse window the entry is dropped, so a code that comes round again
	// much later is not permanently unusable.
	if spent.IsSpent("287082", at.Add(121*time.Second)) {
		t.Error("a spent code should expire out of the blacklist after the window")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}

	return -1
}
