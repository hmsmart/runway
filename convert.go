package main

import (
	"fmt"
	"math"
	"strings"
	"time"

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

// formatDollars renders a non-negative dollar amount for user-facing text:
// comma-grouped, with cents shown only when the amount isn't a whole dollar,
// e.g. "$1,500" or "$1,234.56". Callers prepend their own sign, since sign
// conventions differ (see formatTransactionMessage's debit/credit arrows).
func formatDollars(amount float64) string {
	var num string
	if amount == math.Trunc(amount) {
		num = fmt.Sprintf("%.0f", amount)
	} else {
		num = fmt.Sprintf("%.2f", amount)
	}
	return "$" + addCommas(num)
}

// formatDollarsCents renders a non-negative dollar amount with cents always
// shown, comma-grouped, e.g. "$500.00" or "$1,234.56". Used for transaction
// amounts, where an exact receipt-style figure reads better than
// formatDollars' whole-dollar shorthand.
func formatDollarsCents(amount float64) string {
	return "$" + addCommas(fmt.Sprintf("%.2f", amount))
}

// addCommas inserts thousands separators into the integer part of a decimal
// string, e.g. "1234.56" -> "1,234.56".
func addCommas(s string) string {
	intPart, frac, hasFrac := strings.Cut(s, ".")
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = intPart[1:]
	}
	n := len(intPart)
	var out strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 && (n-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteByte(intPart[i])
	}
	result := out.String()
	if neg {
		result = "-" + result
	}
	if hasFrac {
		result += "." + frac
	}
	return result
}

// humanDate renders a YYYY-MM-DD date (Plaid/SQLite's format) for user-facing
// text, e.g. "Jul 9", or "Jul 9, 2027" when the year isn't the current one.
// Unparseable input passes through rather than being hidden from the user.
func humanDate(s string) string {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return s
	}
	if d.Year() != time.Now().Year() {
		return d.Format("Jan 2, 2006")
	}
	return d.Format("Jan 2")
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
