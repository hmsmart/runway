package main

import (
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// StringPtrOk converts a plaid-style (value, ok) getter result to a *string
// suitable for sqlc's pointer-typed nullable columns.
func StringPtrOk[T ~string](s *T, ok bool) *string {
	if !ok || s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

// stringOr dereferences a nullable string, falling back to a default.
func stringOr(s *string, fallback string) string {
	if s != nil && *s != "" {
		return *s
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
	// Fallback for anything new Plaid adds. The Caser is built per call on
	// purpose: cases.Caser is stateful and not safe for concurrent use.
	s := strings.ReplaceAll(raw, "_", " ")
	return cases.Title(language.English).String(strings.ToLower(s))
}
