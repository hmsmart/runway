package domains

import "time"

// TransactionRow adapts a ListTransactionsByUser row for the /transactions
// table. It derives the spread label (the amort window's last covered day)
// so templates only ever deal with display strings.
type TransactionRow struct {
	date        string
	account     string
	description string
	amount      float64
	excluded    bool
	spread      string
}

// NewTransactionRow builds a display row from the raw query columns. rawDate
// and amortEnd are the unadjusted transaction date and amort_end column
// (nil/before rawDate means the transaction isn't spread) — matching
// SetAmortEnd's date-inclusive, amort_end-exclusive window.
func NewTransactionRow(date, account, description string, amount float64, excluded bool, rawDate string, amortEnd *string) TransactionRow {
	row := TransactionRow{date: date, account: account, description: description, amount: amount, excluded: excluded}
	if amortEnd != nil && *amortEnd > rawDate {
		if end, err := time.Parse(time.DateOnly, *amortEnd); err == nil {
			row.spread = "→ " + end.AddDate(0, 0, -1).Format("Jan 2")
		}
	}
	return row
}

func (t TransactionRow) Date() string        { return t.date }
func (t TransactionRow) Account() string     { return t.account }
func (t TransactionRow) Description() string { return t.description }
func (t TransactionRow) Amount() float64     { return t.amount }
func (t TransactionRow) Excluded() bool      { return t.excluded }

// Spread returns the row's spread label, or "" if the transaction isn't spread.
func (t TransactionRow) Spread() string { return t.spread }
