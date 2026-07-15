package main

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/hmsmart/runway/database"
	"github.com/hmsmart/runway/database/sqlcgen"
)

func TestParseReceiptTransaction(t *testing.T) {
	today := time.Now().Format(time.DateOnly)
	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)

	good := `{"merchant":"Costco Wholesale","amount":128.47,"date":"` + yesterday + `","primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_GROCERIES"}`
	rt, err := parseReceiptTransaction(good)
	if err != nil {
		t.Fatalf("valid json rejected: %v", err)
	}
	if rt.Merchant != "Costco Wholesale" || rt.Amount != 128.47 || rt.Date != yesterday {
		t.Errorf("parsed fields wrong: %+v", rt)
	}

	// A missing date defaults to today rather than erroring: the prompt
	// tells the model to use the current day, but the guarantee lives here.
	noDate := `{"merchant":"X","amount":10,"primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`
	rt, err = parseReceiptTransaction(noDate)
	if err != nil {
		t.Fatalf("dateless json rejected: %v", err)
	}
	if rt.Date != today {
		t.Errorf("missing date = %q, want today %q", rt.Date, today)
	}

	tooOld := time.Now().AddDate(0, 0, -(notifyWindowDays + 1)).Format(time.DateOnly)
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	bad := map[string]string{
		"not json":          `parsed: Costco $128`,
		"no merchant":       `{"merchant":"  ","amount":10,"primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"zero amount":       `{"merchant":"X","amount":0,"primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"negative amount":   `{"merchant":"X","amount":-5,"primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"absurd amount":     `{"merchant":"X","amount":2000000,"primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"invented primary":  `{"merchant":"X","amount":10,"primary_category":"SNACKS","detailed_category":"SNACKS_CHIPS"}`,
		"mismatched detail": `{"merchant":"X","amount":10,"primary_category":"FOOD_AND_DRINK","detailed_category":"TRAVEL_FLIGHTS"}`,
		"garbage date":      `{"merchant":"X","amount":10,"date":"last tuesday","primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"future date":       `{"merchant":"X","amount":10,"date":"` + tomorrow + `","primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
		"too-old date":      `{"merchant":"X","amount":10,"date":"` + tooOld + `","primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_COFFEE"}`,
	}
	for name, raw := range bad {
		if _, err := parseReceiptTransaction(raw); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}

	// The too-old case must be distinguishable so handlers can explain it.
	if _, err := parseReceiptTransaction(bad["too-old date"]); !errors.Is(err, errReceiptTooOld) {
		t.Errorf("too-old date error = %v, want errReceiptTooOld", err)
	}
}

// registerReceiptTestUser creates an active user the FK chain
// (items.user_id) requires.
func registerReceiptTestUser(t *testing.T, store *database.Store) string {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateInviteCode(ctx, sqlcgen.CreateInviteCodeParams{ID: "user-receipt", InviteCode: "ABCD2234"}); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	tgID := int64(99999)
	if _, err := store.RedeemInviteCode(ctx, sqlcgen.RedeemInviteCodeParams{TgID: &tgID, InviteCode: "ABCD2234"}); err != nil {
		t.Fatalf("redeem invite: %v", err)
	}
	return "user-receipt"
}

func TestCreateManualTransaction(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	userID := registerReceiptTestUser(t, store)
	cfg := &Config{DBCryptKey: make([]byte, 32)}
	rand.Read(cfg.DBCryptKey)

	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
	raw := `{"merchant":"Costco Wholesale","amount":128.47,"date":"` + yesterday + `","primary_category":"FOOD_AND_DRINK","detailed_category":"FOOD_AND_DRINK_GROCERIES"}`
	rt, err := parseReceiptTransaction(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	txID, err := createManualTransaction(ctx, store, cfg, userID, rt, raw)
	if err != nil {
		t.Fatalf("create manual transaction: %v", err)
	}

	tx, err := store.GetTransaction(ctx, txID)
	if err != nil {
		t.Fatalf("read back transaction: %v", err)
	}
	if tx.Amount != 128.47 || tx.Name != "Costco Wholesale" || tx.PaymentChannel != "manual" {
		t.Errorf("row fields wrong: %+v", tx)
	}
	if tx.Date != yesterday {
		t.Errorf("date = %s, want the model-extracted date %s", tx.Date, yesterday)
	}
	if tx.Notified != 0 {
		t.Errorf("notified = %d, want 0 so the drain worker announces it", tx.Notified)
	}

	// The drain worker's query must see the row via the manual item's
	// user_id -> tg_id join.
	tgID := int64(99999)
	rows, err := store.GetPendingNotifications(ctx, &tgID)
	if err != nil {
		t.Fatalf("pending notifications: %v", err)
	}
	if len(rows) != 1 || rows[0].Transaction.TxID != txID {
		t.Errorf("pending notifications = %d rows, want the manual tx", len(rows))
	}

	// The spend series must count it: the manual account is tracked.
	spend, err := store.ListSpendTransactionsByUser(ctx, userID)
	if err != nil {
		t.Fatalf("spend transactions: %v", err)
	}
	if len(spend) != 1 || spend[0].Amount != 128.47 {
		t.Errorf("spend rows = %+v, want the manual tx", spend)
	}

	// A second receipt reuses the container instead of failing on a
	// duplicate item.
	if _, err := createManualTransaction(ctx, store, cfg, userID, rt, raw); err != nil {
		t.Fatalf("second manual transaction: %v", err)
	}

	// The manual container stays hidden from the active-item surfaces.
	items, err := store.ListItemsByUser(ctx, userID)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("manual item leaked into active item list: %+v", items)
	}
}
