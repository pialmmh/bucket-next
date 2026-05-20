package datatype

import "testing"

func TestParse_allKnownTypes(t *testing.T) {
	for _, dt := range All {
		got, err := Parse(string(dt))
		if err != nil || got != dt {
			t.Errorf("Parse(%q) = (%q, %v); want (%q, nil)", dt, got, err, dt)
		}
	}
}

func TestParse_rejectsUnknown(t *testing.T) {
	for _, s := range []string{"", "bigint", "INT", "uuid", "uuid32", "snowflake "} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) should error", s)
		}
	}
}

func TestIsNumeric(t *testing.T) {
	yes := []DataType{Int, Long}
	no := []DataType{Snowflake, UUID8, UUID12, UUID16, UUID22}
	for _, d := range yes {
		if !d.IsNumeric() {
			t.Errorf("%s.IsNumeric() = false", d)
		}
	}
	for _, d := range no {
		if d.IsNumeric() {
			t.Errorf("%s.IsNumeric() = true", d)
		}
	}
}

func TestIsUUID(t *testing.T) {
	yes := []DataType{UUID8, UUID12, UUID16, UUID22}
	no := []DataType{Int, Long, Snowflake}
	for _, d := range yes {
		if !d.IsUUID() {
			t.Errorf("%s.IsUUID() = false", d)
		}
	}
	for _, d := range no {
		if d.IsUUID() {
			t.Errorf("%s.IsUUID() = true", d)
		}
	}
}

func TestUUIDLength(t *testing.T) {
	cases := map[DataType]int{
		UUID8: 8, UUID12: 12, UUID16: 16, UUID22: 22,
		Int: 0, Long: 0, Snowflake: 0,
	}
	for dt, want := range cases {
		if got := dt.UUIDLength(); got != want {
			t.Errorf("%s.UUIDLength() = %d, want %d", dt, got, want)
		}
	}
}

func TestMaxValues(t *testing.T) {
	if MaxInt != 2147483647 {
		t.Errorf("MaxInt = %d", MaxInt)
	}
	if MaxLong != 9223372036854775807 {
		t.Errorf("MaxLong = %d", MaxLong)
	}
}
