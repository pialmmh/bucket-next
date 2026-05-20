package shortid

import (
	"strings"
	"testing"
)

func TestGenerate_lengthAndCharset(t *testing.T) {
	for _, n := range []int{1, 8, 12, 16, 22, 100} {
		s, err := Generate(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != n {
			t.Errorf("Generate(%d) len=%d", n, len(s))
		}
		for _, c := range s {
			if !strings.ContainsRune(charset, c) {
				t.Errorf("char %q not in charset", c)
			}
		}
	}
}

func TestGenerate_rejectsNonPositive(t *testing.T) {
	if _, err := Generate(0); err == nil {
		t.Error("Generate(0) should error")
	}
	if _, err := Generate(-5); err == nil {
		t.Error("Generate(-5) should error")
	}
}

func TestGenerateBatch(t *testing.T) {
	batch, err := GenerateBatch(12, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 50 {
		t.Errorf("batch len=%d, want 50", len(batch))
	}
	for i, s := range batch {
		if len(s) != 12 {
			t.Errorf("batch[%d] len=%d, want 12", i, len(s))
		}
	}
}

func TestGenerate_distribution(t *testing.T) {
	// Crude uniqueness check: 1000 16-char IDs should all be unique with crypto-rand.
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		s, _ := Generate(16)
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate at i=%d: %s", i, s)
		}
		seen[s] = struct{}{}
	}
}
