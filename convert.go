package main

import (
	"database/sql"
	"fmt"
	"time"
)

func ToNullString[T ~string](s *T, ok bool) sql.NullString {
	if !ok || s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(*s), Valid: true}
}

func ToNullInt64(i *int64, ok bool) sql.NullInt64 {
	if !ok || i == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *i, Valid: true}
}

func ToNullFloat64(f *float64, ok bool) sql.NullFloat64 {
	if !ok || f == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *f, Valid: true}
}

func ToNullTime(t *time.Time, ok bool) sql.NullTime {
	if !ok || t == nil {
		fmt.Printf("Null Time??")
		return sql.NullTime{}
	}
	fmt.Printf("At Time: %v", *t)
	return sql.NullTime{Time: *t, Valid: ok}
}
