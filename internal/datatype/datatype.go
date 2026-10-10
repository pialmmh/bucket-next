package datatype

import "fmt"

type DataType string

const (
	Int       DataType = "int"
	Long      DataType = "long"
	Snowflake DataType = "snowflake"
	UUID8     DataType = "uuid8"
	UUID12    DataType = "uuid12"
	UUID16    DataType = "uuid16"
	UUID22    DataType = "uuid22"
)

const (
	MaxInt  int64 = 2147483647
	MaxLong int64 = 9223372036854775807
	// MaxUUID8 is 62^8 - 1: the largest counter value an 8-char base62 string can carry.
	MaxUUID8 int64 = 218340105584895
)

var All = []DataType{Int, Long, Snowflake, UUID8, UUID12, UUID16, UUID22}

func Parse(s string) (DataType, error) {
	for _, d := range All {
		if string(d) == s {
			return d, nil
		}
	}
	return "", fmt.Errorf("invalid dataType %q; valid: %v", s, All)
}

// IsNumeric reports the types whose IDs are plain sequential numbers.
func (d DataType) IsNumeric() bool {
	return d == Int || d == Long
}

func (d DataType) IsUUID() bool {
	return d == UUID8 || d == UUID12 || d == UUID16 || d == UUID22
}

// IsCounter reports the types backed by a per-entity counter (and so by the allocator):
// int, long and every uuid type. Snowflake IDs come from the clock instead.
func (d DataType) IsCounter() bool {
	return d.IsNumeric() || d.IsUUID()
}

func (d DataType) UUIDLength() int {
	switch d {
	case UUID8:
		return 8
	case UUID12:
		return 12
	case UUID16:
		return 16
	case UUID22:
		return 22
	}
	return 0
}

// MaxValue is the largest counter value an entity of this type can reach (0 for snowflake).
func (d DataType) MaxValue() int64 {
	switch d {
	case Int:
		return MaxInt
	case UUID8:
		return MaxUUID8
	case Long, UUID12, UUID16, UUID22:
		return MaxLong
	}
	return 0
}
