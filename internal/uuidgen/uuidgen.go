// Package uuidgen turns a unique counter value into a fixed-length alphanumeric string.
//
// The mapping is a keyed permutation (a Feistel network with AES as the round function),
// so two different counter values always give two different strings, while the output
// looks random to anyone who does not hold the key. Uniqueness comes from the counter;
// the key only adds unpredictability.
//
// The entity name is mixed in as a tweak: every entity gets its own permutation, so two
// entities whose counters are at the same value still get unrelated strings. Guaranteed
// uniqueness is per entity (counter + bijection); across entities it is statistical.
//
//	length 8        value < 62^8. 48-bit Feistel with cycle walking over exactly [0, 62^8),
//	                so every 8-char string is a possible output.
//	length 12/16/22 value < 2^63. 64-bit Feistel gives the low 64 bits; a keyed pad fills
//	                the rest so all character positions vary. The low 64 bits stay unique.
package uuidgen

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Alphabet is the base62 digit order used by math/big for base 62.
const Alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const (
	rounds = 8
	tag64  = 0x64
	tag48  = 0x48
	tagPad = 0x50

	mask32 = 0xffffffff
	mask24 = 0xffffff
)

// domain8 is 62^8, the number of distinct 8-char strings.
const domain8 uint64 = 218340105584896

const defaultKeySeed = "bucket-next/uuidgen/default-key/v1"

var two64 = new(big.Int).Lsh(big.NewInt(1), 64)

// DefaultKey is the built-in key used when the config sets no uuid_key. IDs stay unique;
// they are just predictable to anyone who has this source code.
func DefaultKey() []byte {
	sum := sha256.Sum256([]byte(defaultKeySeed))
	return sum[:16]
}

// Fingerprint is a short, non-secret identifier of a key, stored in the state file so a
// changed key is detected at startup.
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// SupportedLength reports the lengths this package encodes.
func SupportedLength(length int) bool {
	return length == 8 || length == 12 || length == 16 || length == 22
}

// Tweak selects one entity's permutation. Build it once per entity with TweakFor.
type Tweak [6]byte

// TweakFor derives the tweak for an entity name.
func TweakFor(entity string) Tweak {
	sum := sha256.Sum256([]byte(entity))
	var t Tweak
	copy(t[:], sum[:6])
	return t
}

type Codec struct {
	block cipher.Block
	fp    string
	limit map[int]*big.Int // floor(62^L / 2^64) for L = 12, 16, 22
}

// New builds a codec from a 16, 24 or 32 byte AES key.
func New(key []byte) (*Codec, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("uuid key: %w", err)
	}
	c := &Codec{block: block, fp: Fingerprint(key), limit: map[int]*big.Int{}}
	for _, l := range []int{12, 16, 22} {
		p := new(big.Int).Exp(big.NewInt(62), big.NewInt(int64(l)), nil)
		c.limit[l] = p.Div(p, two64)
	}
	return c, nil
}

func (c *Codec) Fingerprint() string { return c.fp }

// Encode maps a counter value to a string of exactly length characters, using the entity's
// tweak.
func (c *Codec) Encode(v int64, length int, tw Tweak) (string, error) {
	if v < 0 {
		return "", errors.New("uuid value must be >= 0")
	}
	switch length {
	case 8:
		x := uint64(v)
		if x >= domain8 {
			return "", fmt.Errorf("value %d exceeds uuid8 capacity (%d)", v, domain8-1)
		}
		y := c.perm48(x, tw)
		for y >= domain8 {
			y = c.perm48(y, tw)
		}
		return encodeFixed(y, 8), nil
	case 12, 16, 22:
		y := c.perm64(uint64(v), tw)
		z := new(big.Int).Lsh(c.pad(uint64(v), length, tw), 64)
		z.Or(z, new(big.Int).SetUint64(y))
		s := z.Text(62)
		if len(s) < length {
			s = strings.Repeat("0", length-len(s)) + s
		}
		return s, nil
	}
	return "", fmt.Errorf("unsupported uuid length %d", length)
}

// Decode reverses Encode for the same entity tweak. It rejects strings this codec could not
// have produced where it can tell (wrong length, wrong characters, or — for lengths
// 12/16/22 — a pad mismatch, e.g. a string from another entity or key).
func (c *Codec) Decode(s string, length int, tw Tweak) (int64, error) {
	if !SupportedLength(length) {
		return 0, fmt.Errorf("unsupported uuid length %d", length)
	}
	if len(s) != length {
		return 0, fmt.Errorf("want %d chars, got %d", length, len(s))
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(Alphabet, s[i]) < 0 {
			return 0, fmt.Errorf("invalid character %q", s[i])
		}
	}
	if length == 8 {
		y := decodeFixed(s)
		if y >= domain8 {
			return 0, errors.New("value out of range")
		}
		x := c.unperm48(y, tw)
		for x >= domain8 {
			x = c.unperm48(x, tw)
		}
		return int64(x), nil
	}
	z, ok := new(big.Int).SetString(s, 62)
	if !ok {
		return 0, errors.New("not base62")
	}
	pad := new(big.Int).Rsh(z, 64)
	y := new(big.Int).And(z, new(big.Int).Sub(two64, big.NewInt(1))).Uint64()
	x := c.unperm64(y, tw)
	if x > math.MaxInt64 || pad.Cmp(c.pad(x, length, tw)) != 0 {
		return 0, errors.New("not issued by this key")
	}
	return int64(x), nil
}

// round is the Feistel round function: AES of (tag, round, tweak, half), truncated by the
// caller.
func (c *Codec) round(tag byte, i int, tw Tweak, half uint64) uint64 {
	var in, out [16]byte
	in[0], in[1] = tag, byte(i)
	copy(in[2:8], tw[:])
	binary.BigEndian.PutUint64(in[8:], half)
	c.block.Encrypt(out[:], in[:])
	return binary.BigEndian.Uint64(out[:8])
}

func (c *Codec) perm64(x uint64, tw Tweak) uint64 {
	l, r := x>>32, x&mask32
	for i := 0; i < rounds; i++ {
		l, r = r, l^(c.round(tag64, i, tw, r)&mask32)
	}
	return l<<32 | r
}

func (c *Codec) unperm64(y uint64, tw Tweak) uint64 {
	l, r := y>>32, y&mask32
	for i := rounds - 1; i >= 0; i-- {
		l, r = r^(c.round(tag64, i, tw, l)&mask32), l
	}
	return l<<32 | r
}

func (c *Codec) perm48(x uint64, tw Tweak) uint64 {
	l, r := x>>24, x&mask24
	for i := 0; i < rounds; i++ {
		l, r = r, l^(c.round(tag48, i, tw, r)&mask24)
	}
	return l<<24 | r
}

func (c *Codec) unperm48(y uint64, tw Tweak) uint64 {
	l, r := y>>24, y&mask24
	for i := rounds - 1; i >= 0; i-- {
		l, r = r^(c.round(tag48, i, tw, l)&mask24), l
	}
	return l<<24 | r
}

// pad is a keyed value in [0, limit) that fills the characters above the low 64 bits.
func (c *Codec) pad(x uint64, length int, tw Tweak) *big.Int {
	var in, out [16]byte
	in[0], in[1] = tagPad, byte(length)
	copy(in[2:8], tw[:])
	binary.BigEndian.PutUint64(in[8:], x)
	c.block.Encrypt(out[:], in[:])
	p := new(big.Int).SetBytes(out[:])
	return p.Mod(p, c.limit[length])
}

func encodeFixed(y uint64, length int) string {
	b := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		b[i] = Alphabet[y%62]
		y /= 62
	}
	return string(b)
}

func decodeFixed(s string) uint64 {
	var y uint64
	for i := 0; i < len(s); i++ {
		y = y*62 + uint64(strings.IndexByte(Alphabet, s[i]))
	}
	return y
}
