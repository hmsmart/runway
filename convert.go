package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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

func NullStringToPtr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

// stringOr unwraps a NullString, falling back to a default.
func stringOr(ns sql.NullString, fallback string) string {
	if ns.Valid && ns.String != "" {
		return ns.String
	}
	return fallback
}

var categoryDisplay = map[string]string{
	"INCOME":                    "Income",
	"TRANSFER_IN":               "Transfer In",
	"TRANSFER_OUT":              "Transfer Out",
	"LOAN_PAYMENTS":             "Loan Payments",
	"BANK_FEES":                 "Bank Fees",
	"ENTERTAINMENT":             "Entertainment",
	"FOOD_AND_DRINK":            "Food & Drink",
	"GENERAL_MERCHANDISE":       "General Merchandise",
	"HOME_IMPROVEMENT":          "Home Improvement",
	"MEDICAL":                   "Medical",
	"PERSONAL_CARE":             "Personal Care",
	"GENERAL_SERVICES":          "General Services",
	"GOVERNMENT_AND_NON_PROFIT": "Government & Non-Profit",
	"TRANSPORTATION":            "Transportation",
	"TRAVEL":                    "Travel",
	"RENT_AND_UTILITIES":        "Rent & Utilities",
}

func displayCategory(raw string) string {
	if d, ok := categoryDisplay[raw]; ok {
		return d
	}
	// Fallback for anything new Plaid adds
	s := strings.ReplaceAll(raw, "_", " ")
	return cases.Title(language.English).String(strings.ToLower(s))
}
