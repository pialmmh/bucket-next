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

func (d DataType) IsNumeric() bool {
	return d == Int || d == Long
}

func (d DataType) IsUUID() bool {
	return d == UUID8 || d == UUID12 || d == UUID16 || d == UUID22
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
