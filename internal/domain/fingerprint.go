package domain

import (
	"crypto/sha256"
	"encoding/hex"
)

// longValueLength is the length from which a literal is identified by hash
// alone, so equal long values group across variable names.
const longValueLength = 20

// hash16 returns the first 16 hex characters of the SHA-256 of value.
func hash16(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// Fingerprint identifies a value without storing it:
// a sealed variable becomes "sealed:<serviceID|shared>:<envID>:<name>"; a
// reference becomes "ref:<name>:<hash>" of the unrendered text; a literal of
// 20+ characters becomes its bare hash; a shorter literal becomes
// "<name>:<hash>". The value itself is never part of the result.
func Fingerprint(name, value string, sealed, literal bool, envID, serviceID string) string {
	if sealed {
		scope := serviceID
		if scope == "" {
			scope = "shared"
		}
		return "sealed:" + scope + ":" + envID + ":" + name
	}
	if !literal {
		return "ref:" + name + ":" + hash16(value)
	}
	if len(value) >= longValueLength {
		return hash16(value)
	}
	return name + ":" + hash16(value)
}
