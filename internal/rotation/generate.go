package rotation

import (
	"crypto/rand"
	"math/big"
	"strings"
)

const (
	hexAlphabet = "0123456789abcdef"
	alnum       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	minGenLen   = 24
)

// Generate returns a new random value of length max(oldLen, 24): lowercase
// hex when hex, else ASCII letters and digits.
func Generate(oldLen int, hex bool) (string, error) {
	n := max(oldLen, minGenLen)
	alphabet := alnum
	if hex {
		alphabet = hexAlphabet
	}
	limit := big.NewInt(int64(len(alphabet)))
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		k, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(alphabet[k.Int64()])
	}
	return b.String(), nil
}
