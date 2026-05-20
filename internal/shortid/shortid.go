package shortid

import (
	"crypto/rand"
	"fmt"
)

const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Generate returns a random alphanumeric string of the given length using crypto/rand.
func Generate(length int) (string, error) {
	if length < 1 {
		return "", fmt.Errorf("length must be >= 1")
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		out[i] = charset[int(buf[i])%len(charset)]
	}
	return string(out), nil
}

// GenerateBatch returns n random strings of the given length.
func GenerateBatch(length, n int) ([]string, error) {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		s, err := Generate(length)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
