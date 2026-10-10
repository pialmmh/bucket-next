package uuidgen

import (
	"math"
	"strings"
	"testing"
)

func codec(t *testing.T) *Codec {
	t.Helper()
	c, err := New(DefaultKey())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var lengths = []int{8, 12, 16, 22}

var tw = TweakFor("test-entity")

func TestEncode_uniqueFixedLengthRoundTrip(t *testing.T) {
	c := codec(t)
	for _, l := range lengths {
		seen := make(map[string]int64, 50000)
		for v := int64(0); v < 50000; v++ {
			s, err := c.Encode(v, l, tw)
			if err != nil {
				t.Fatalf("len %d v %d: %v", l, v, err)
			}
			if len(s) != l {
				t.Fatalf("len %d: got %d chars (%q)", l, len(s), s)
			}
			for _, ch := range s {
				if !strings.ContainsRune(Alphabet, ch) {
					t.Fatalf("len %d: bad char %q in %q", l, ch, s)
				}
			}
			if prev, dup := seen[s]; dup {
				t.Fatalf("len %d: %q issued for %d and %d", l, s, prev, v)
			}
			seen[s] = v
			back, err := c.Decode(s, l, tw)
			if err != nil || back != v {
				t.Fatalf("len %d: decode(%q) = %d, %v; want %d", l, s, back, err, v)
			}
		}
	}
}

func TestEncode_largeValues(t *testing.T) {
	c := codec(t)
	for _, l := range []int{12, 16, 22} {
		for _, v := range []int64{math.MaxInt64, math.MaxInt64 - 1, 1 << 40} {
			s, err := c.Encode(v, l, tw)
			if err != nil {
				t.Fatal(err)
			}
			if back, err := c.Decode(s, l, tw); err != nil || back != v {
				t.Errorf("len %d v %d: decode=%d err=%v", l, v, back, err)
			}
		}
	}
}

func TestUUID8_capacity(t *testing.T) {
	c := codec(t)
	max := int64(domain8 - 1)
	s, err := c.Encode(max, 8, tw)
	if err != nil {
		t.Fatalf("62^8-1 should encode: %v", err)
	}
	if back, _ := c.Decode(s, 8, tw); back != max {
		t.Errorf("round trip at capacity: %d", back)
	}
	if _, err := c.Encode(max+1, 8, tw); err == nil {
		t.Error("62^8 should not encode")
	}
}

func TestEncode_looksRandom(t *testing.T) {
	// Sequential inputs must not give sequential or same-prefixed outputs.
	c := codec(t)
	for _, l := range lengths {
		first := map[byte]bool{}
		for v := int64(0); v < 5000; v++ {
			s, _ := c.Encode(v, l, tw)
			first[s[0]] = true
		}
		if len(first) < 40 {
			t.Errorf("len %d: only %d distinct first characters over 5000 IDs", l, len(first))
		}
	}
}

func TestDifferentKeysDifferentOutput(t *testing.T) {
	a := codec(t)
	other := make([]byte, 16)
	copy(other, "another-key-1234")
	b, _ := New(other)
	same := 0
	for v := int64(0); v < 1000; v++ {
		x, _ := a.Encode(v, 12, tw)
		y, _ := b.Encode(v, 12, tw)
		if x == y {
			same++
		}
	}
	if same > 0 {
		t.Errorf("%d identical outputs across keys", same)
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("fingerprints should differ")
	}
}

func TestDecode_rejectsBadInput(t *testing.T) {
	c := codec(t)
	good, _ := c.Encode(42, 16, tw)
	tampered := []byte(good)
	tampered[0] = map[bool]byte{true: 'a', false: 'b'}[tampered[0] == 'b']
	bad := []struct {
		s string
		l int
	}{
		{"short", 8}, {"abc!defg", 8}, {good, 12}, {string(tampered), 16}, {"x", 9},
	}
	for _, b := range bad {
		if _, err := c.Decode(b.s, b.l, tw); err == nil {
			t.Errorf("Decode(%q, %d) should fail", b.s, b.l)
		}
	}
}

func TestNew_rejectsBadKey(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Error("5-byte key should be rejected")
	}
}

func BenchmarkEncode8(b *testing.B) {
	c, _ := New(DefaultKey())
	for i := 0; i < b.N; i++ {
		c.Encode(int64(i), 8, tw)
	}
}

func BenchmarkEncode16(b *testing.B) {
	c, _ := New(DefaultKey())
	for i := 0; i < b.N; i++ {
		c.Encode(int64(i), 16, tw)
	}
}

func TestEntitiesGetUnrelatedSequences(t *testing.T) {
	c := codec(t)
	a, b := TweakFor("session"), TweakFor("token")
	for _, l := range lengths {
		same := 0
		for v := int64(0); v < 2000; v++ {
			x, _ := c.Encode(v, l, a)
			y, _ := c.Encode(v, l, b)
			if x == y {
				same++
			}
		}
		if same > 0 {
			t.Errorf("len %d: %d identical strings for two entities at the same counter", l, same)
		}
	}
	// A string decodes only under its own entity (lengths 12+ carry a check).
	s, _ := c.Encode(7, 16, a)
	if _, err := c.Decode(s, 16, b); err == nil {
		t.Error("decoding with another entity's tweak should fail")
	}
}
