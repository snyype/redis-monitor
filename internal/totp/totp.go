// Package totp is a dependency-free RFC 6238 (TOTP) / RFC 4226 (HOTP)
// implementation: SHA1, 6 digits, 30 second period — the defaults every
// authenticator app uses.
//
// Ported from the TotpHelper the PHP monitor authorises deletes with, so the same
// enrolled authenticator works against either implementation.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// Period is the TOTP time step.
	Period = 30 * time.Second

	// Digits is the length of a generated code.
	Digits = 6
)

// GenerateSecret returns a random, unpadded base32 shared secret.
func GenerateSecret(bytes int) (string, error) {
	if bytes < 1 {
		bytes = 20
	}

	buffer := make([]byte, bytes)

	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}

	return base32Encoding.EncodeToString(buffer), nil
}

// Verify checks a user-supplied code against the shared secret, allowing window
// steps of clock drift on each side. window=1 tolerates ±30s, the same as login.
func Verify(secret, code string, window int, at time.Time) bool {
	code = strings.TrimSpace(code)

	if !isSixDigits(code) {
		return false
	}

	key, err := decodeSecret(secret)
	if err != nil || len(key) == 0 {
		return false
	}

	if window < 0 {
		window = 0
	}

	counter := at.Unix() / int64(Period.Seconds())

	for offset := -window; offset <= window; offset++ {
		candidate := hotp(key, counter+int64(offset))

		// Constant-time so a wrong code cannot be narrowed down by timing.
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return true
		}
	}

	return false
}

// ProvisioningURI is the otpauth:// URI an authenticator app scans.
func ProvisioningURI(secret, account, issuer string) string {
	query := url.Values{}
	query.Set("secret", secret)
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", fmt.Sprint(Digits))
	query.Set("period", fmt.Sprint(int(Period.Seconds())))

	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)

	return "otpauth://totp/" + label + "?" + query.Encode()
}

// base32Encoding is RFC 4648 base32 without padding, which is what authenticator
// apps expect.
var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func decodeSecret(secret string) ([]byte, error) {
	cleaned := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "").Replace(strings.TrimSpace(secret)))

	if cleaned == "" {
		return nil, fmt.Errorf("empty secret")
	}

	return base32Encoding.DecodeString(cleaned)
}

func hotp(key []byte, counter int64) string {
	message := make([]byte, 8)
	binary.BigEndian.PutUint64(message, uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(message)
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 section 5.3.
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	modulus := uint32(1)
	for index := 0; index < Digits; index++ {
		modulus *= 10
	}

	return fmt.Sprintf("%0*d", Digits, value%modulus)
}

func isSixDigits(code string) bool {
	if len(code) != Digits {
		return false
	}

	for _, char := range code {
		if char < '0' || char > '9' {
			return false
		}
	}

	return true
}

// SpentCodes remembers codes that have already authorised something.
//
// A TOTP code stays valid for its whole 30 second window (60 with drift), which is
// long enough to replay. Burning a code after it succeeds means the same six digits
// cannot authorise a second batch of deletes — the caller has to wait for their
// authenticator to show the next one.
type SpentCodes struct {
	mu     sync.Mutex
	window time.Duration
	spent  map[string]time.Time
}

func NewSpentCodes(window time.Duration) *SpentCodes {
	return &SpentCodes{window: window, spent: make(map[string]time.Time)}
}

// IsSpent reports whether this code has already been used inside the window.
func (s *SpentCodes) IsSpent(code string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.prune(at)

	_, ok := s.spent[fingerprint(code)]

	return ok
}

// Burn marks a code as used.
func (s *SpentCodes) Burn(code string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.prune(at)
	s.spent[fingerprint(code)] = at.Add(s.window)
}

func (s *SpentCodes) prune(at time.Time) {
	for code, expiresAt := range s.spent {
		if at.After(expiresAt) {
			delete(s.spent, code)
		}
	}
}

// fingerprint keeps the code itself out of memory, the way the PHP version keeps
// it out of the cache key.
func fingerprint(code string) string {
	sum := sha256.Sum256([]byte(code))

	return hex.EncodeToString(sum[:])
}
