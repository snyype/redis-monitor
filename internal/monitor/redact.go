package monitor

import "regexp"

const redactedPlaceholder = "[REDACTED]"

// Pair is one field/value row of a hash or sorted-set preview.
type Pair struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// redactor masks sensitive material on the way out. Two independent layers, both
// needed:
//
//	key patterns   — a key whose NAME looks sensitive is never previewed at all.
//	                 The table still shows its type, TTL and size; the value is
//	                 simply not read back.
//	value patterns — applied to every previewed value, so a token embedded in an
//	                 otherwise harmless cache payload is masked too.
type redactor struct {
	keyPatterns   []*regexp.Regexp
	valuePatterns []*regexp.Regexp
}

func newRedactor(keyPatterns, valuePatterns []*regexp.Regexp) redactor {
	return redactor{keyPatterns: keyPatterns, valuePatterns: valuePatterns}
}

// IsSensitiveKey reports whether this key's value must never be returned.
func (r redactor) IsSensitiveKey(key string) bool {
	for _, pattern := range r.keyPatterns {
		if pattern.MatchString(key) {
			return true
		}
	}

	return false
}

// Value masks every sensitive fragment inside a previewed value.
func (r redactor) Value(value string) string {
	for _, pattern := range r.valuePatterns {
		value = pattern.ReplaceAllString(value, redactedPlaceholder)
	}

	return value
}

// Pair masks a hash field's value when the FIELD NAME is the sensitive part.
//
// The value patterns only fire on "password=x" style text. In a hash the field and
// the value arrive as two separate strings, so a field literally called "password"
// would otherwise hand its value over untouched.
func (r redactor) Pair(field, value string) Pair {
	if r.IsSensitiveKey(field) {
		return Pair{Field: r.Value(field), Value: redactedPlaceholder}
	}

	return Pair{Field: r.Value(field), Value: r.Value(value)}
}

// Values masks a list of previewed elements.
func (r redactor) Values(items []string) []string {
	masked := make([]string, len(items))

	for index, item := range items {
		masked[index] = r.Value(item)
	}

	return masked
}
