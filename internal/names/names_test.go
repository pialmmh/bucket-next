package names

import (
	"strings"
	"testing"
)

func TestValidTenant(t *testing.T) {
	good := []string{"btcl", "link3", "ab", "a" + strings.Repeat("b", 15)}
	bad := []string{"", "b", "BTCL", "Btcl", "3btcl", "bt_cl", "bt-cl", "bt.cl", "bt cl", "a" + strings.Repeat("b", 16), "btcl/x"}
	for _, s := range good {
		if !ValidTenant(s) {
			t.Errorf("%q must be a valid tenant", s)
		}
	}
	for _, s := range bad {
		if ValidTenant(s) {
			t.Errorf("%q must be refused as a tenant", s)
		}
	}
}

func TestValidEntity(t *testing.T) {
	good := []string{"partner.btcl", "cdr", "a", "A-b_c.9", strings.Repeat("x", 64)}
	bad := []string{"", "a/b", "a b", "a?b", "ü", strings.Repeat("x", 65)}
	for _, s := range good {
		if !ValidEntity(s) {
			t.Errorf("%q must be a valid entity", s)
		}
	}
	for _, s := range bad {
		if ValidEntity(s) {
			t.Errorf("%q must be refused as an entity", s)
		}
	}
}
