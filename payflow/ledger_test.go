package payflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tempLedger(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ledger.json")
}

func intent(id, account, paymaster, asset string, amount int64, nonce uint64, state string) Intent {
	return Intent{
		ID:        id,
		Account:   account,
		Paymaster: paymaster,
		Asset:     asset,
		Amount:    amount,
		Nonce:     nonce,
		State:     state,
	}
}

func initBalances(t *testing.T, path string, balances []Balance) {
	t.Helper()
	if err := Init(path, balances); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

func openLedger(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

// ---- Init ----

func TestInitCreatesLedger(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{
		{Account: "aa-1", Asset: "usdc", Balance: 1000},
		{Account: "aa-2", Asset: "usdc", Balance: 500},
	})
	l := openLedger(t, path)
	defer l.Close()
	balances := l.Balances()
	if len(balances) != 2 {
		t.Fatalf("balances = %d, want 2", len(balances))
	}
	if balances[0].Balance != 1000 || balances[1].Balance != 500 {
		t.Fatalf("balances = %+v", balances)
	}
}

func TestInitRejectsDuplicate(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	err := Init(path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 9999}})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second Init = %v, want ErrExists", err)
	}
	// Original balance must be untouched.
	l := openLedger(t, path)
	defer l.Close()
	if got := l.Balances()[0].Balance; got != 1000 {
		t.Fatalf("balance after duplicate init = %d, want 1000", got)
	}
}

func TestInitRejectsInvalidAndCreatesNothing(t *testing.T) {
	cases := []struct {
		name     string
		balances []Balance
	}{
		{"empty account", []Balance{{Account: "", Asset: "usdc", Balance: 10}}},
		{"empty asset", []Balance{{Account: "aa-1", Asset: "", Balance: 10}}},
		{"negative balance", []Balance{{Account: "aa-1", Asset: "usdc", Balance: -1}}},
		{"duplicate pair", []Balance{
			{Account: "aa-1", Asset: "usdc", Balance: 10},
			{Account: "aa-1", Asset: "usdc", Balance: 20},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempLedger(t)
			err := Init(path, tc.balances)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Init = %v, want ErrInvalid", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("ledger file created after invalid init")
			}
			if _, err := os.Stat(lockPath(path)); !os.IsNotExist(err) {
				t.Fatalf("lock file created after invalid init")
			}
		})
	}
}

func TestInitZeroBalanceAllowed(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 0}})
	l := openLedger(t, path)
	defer l.Close()
	if got := l.Balances()[0].Balance; got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

// ---- Submit: basic behavior ----

func TestSubmitSettlesAndDeducts(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 10000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 1500, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	r := results[0]
	if r.Status != StatusSettled {
		t.Fatalf("status = %q, want settled", r.Status)
	}
	// fee = 1500*30/10000 = 4, total = 1504
	if r.Charged != 1504 || r.FeeBps != 30 || r.Ref != "settle:pay-1" {
		t.Fatalf("result = %+v", r)
	}
	if got := l.Balances()[0].Balance; got != 8496 {
		t.Fatalf("balance = %d, want 8496", got)
	}
}

func TestSubmitEmptyBatch(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %d, want 0", len(results))
	}
}

func TestSubmitResultsAlignedToInputOrder(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 10000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending"),
		intent("pay-2", "aa-1", "pm-1", "usdc", 200, 2, "pending"),
		intent("pay-3", "aa-1", "pm-1", "usdc", 300, 3, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	want := []string{"pay-1", "pay-2", "pay-3"}
	for i, r := range results {
		if r.Intent != want[i] {
			t.Fatalf("result[%d].Intent = %q, want %q", i, r.Intent, want[i])
		}
		if r.Status != StatusSettled {
			t.Fatalf("result[%d].Status = %q, want settled", i, r.Status)
		}
	}
	if got := l.Balances()[0].Balance; got != 9400 {
		t.Fatalf("balance = %d, want 9400", got)
	}
}

func TestSubmitLaterItemSeesEarlierSuccess(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 500}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 300, 1, "pending"),
		intent("pay-2", "aa-1", "pm-1", "usdc", 300, 2, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusSettled {
		t.Fatalf("pay-1 status = %q, want settled", results[0].Status)
	}
	if results[1].Status != StatusInsufficient {
		t.Fatalf("pay-2 status = %q, want insufficient", results[1].Status)
	}
	if got := l.Balances()[0].Balance; got != 200 {
		t.Fatalf("balance = %d, want 200", got)
	}
}

func TestSubmitFailedItemDoesNotAffectOthers(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending"),
		intent("pay-bad", "missing", "pm-1", "usdc", 100, 2, "pending"),
		intent("pay-3", "aa-1", "pm-1", "usdc", 100, 3, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusSettled {
		t.Fatalf("pay-1 = %q", results[0].Status)
	}
	if results[1].Status != StatusInsufficient {
		t.Fatalf("pay-bad = %q, want insufficient", results[1].Status)
	}
	if results[2].Status != StatusSettled {
		t.Fatalf("pay-3 = %q, want settled", results[2].Status)
	}
	if got := l.Balances()[0].Balance; got != 800 {
		t.Fatalf("balance = %d, want 800", got)
	}
}

// ---- Fee rounding ----

func TestFeeRoundingDown(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	// 10000 * 30 / 10000 = 30 exactly; 9999 * 30 / 10000 = 29 (round down)
	results, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 9999, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Charged != 10028 {
		t.Fatalf("charged = %d, want 10028 (fee 29)", results[0].Charged)
	}
}

func TestFeeZeroAndMax(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 10000, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// fee = 100*10000/10000 = 100, total = 200
	if results[0].Charged != 200 {
		t.Fatalf("charged = %d, want 200", results[0].Charged)
	}
}

// ---- Parameter validation ----

func TestSubmitRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name string
		in   Intent
		fee  int
	}{
		{"empty id", Intent{ID: "", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: 100, Nonce: 1, State: "pending"}, 0},
		{"empty account", Intent{ID: "pay-1", Account: "", Paymaster: "pm-1", Asset: "usdc", Amount: 100, Nonce: 1, State: "pending"}, 0},
		{"empty asset", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Asset: "", Amount: 100, Nonce: 1, State: "pending"}, 0},
		{"empty paymaster", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "", Asset: "usdc", Amount: 100, Nonce: 1, State: "pending"}, 0},
		{"zero amount", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: 0, Nonce: 1, State: "pending"}, 0},
		{"negative amount", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: -5, Nonce: 1, State: "pending"}, 0},
		{"fee too high", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: 100, Nonce: 1, State: "pending"}, 10001},
		{"fee negative", Intent{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Asset: "usdc", Amount: 100, Nonce: 1, State: "pending"}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tempLedger(t)
			initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
			l := openLedger(t, path)
			defer l.Close()
			results, err := l.Submit(Batch{FeeBps: tc.fee, Intents: []Intent{tc.in}})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if results[0].Status != StatusRejected {
				t.Fatalf("status = %q, want rejected", results[0].Status)
			}
			if got := l.Balances()[0].Balance; got != 100000 {
				t.Fatalf("balance = %d, want 100000 (unchanged)", got)
			}
		})
	}
}

func TestSubmitRejectsOverflow(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	// MaxInt64 * 10000 overflows int64.
	results, err := l.Submit(Batch{FeeBps: 10000, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 1<<62, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusRejected {
		t.Fatalf("status = %q, want rejected", results[0].Status)
	}
}

// ---- State errors ----

func TestSubmitNonPendingStateError(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	for _, state := range []string{"settled", "failed", "cancelled", ""} {
		results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
			intent("pay-"+state, "aa-1", "pm-1", "usdc", 100, 1, state),
		}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if results[0].Status != StatusStateError {
			t.Fatalf("state=%q status = %q, want state_error", state, results[0].Status)
		}
	}
	if got := l.Balances()[0].Balance; got != 100000 {
		t.Fatalf("balance = %d, want 100000", got)
	}
}

// ---- Insufficient balance ----

func TestSubmitMissingAccountAssetIsInsufficient(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-1", "aa-2", "pm-1", "usdc", 100, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusInsufficient {
		t.Fatalf("status = %q, want insufficient", results[0].Status)
	}
}

func TestSubmitInsufficientLeavesNoRecord(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 50}})
	l := openLedger(t, path)
	defer l.Close()
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusInsufficient {
		t.Fatalf("status = %q", results[0].Status)
	}
	if len(l.Records()) != 0 {
		t.Fatalf("records = %d, want 0", len(l.Records()))
	}
	if got := l.Balances()[0].Balance; got != 50 {
		t.Fatalf("balance = %d, want 50", got)
	}
}

// ---- Idempotency / dedup ----

func TestDuplicateIdenticalFieldsReplays(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	in := intent("pay-1", "aa-1", "pm-1", "usdc", 1500, 1, "pending")
	first, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{in}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if first[0].Status != StatusSettled {
		t.Fatalf("first = %q", first[0].Status)
	}
	balanceAfterFirst := l.Balances()[0].Balance

	// Resubmit identical fields (state can be pending again).
	dup := in
	dup.State = "pending"
	second, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{dup}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if second[0].Status != StatusDuplicate {
		t.Fatalf("duplicate status = %q, want duplicate", second[0].Status)
	}
	if second[0].Charged != first[0].Charged || second[0].Ref != first[0].Ref {
		t.Fatalf("duplicate result = %+v, want original %+v", second[0], first[0])
	}
	if got := l.Balances()[0].Balance; got != balanceAfterFirst {
		t.Fatalf("balance changed on duplicate: %d, want %d", got, balanceAfterFirst)
	}
	if len(l.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(l.Records()))
	}
}

func TestDuplicateDifferentFieldsConflicts(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	base := intent("pay-1", "aa-1", "pm-1", "usdc", 1500, 1, "pending")
	if _, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{base}}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	balanceAfterFirst := l.Balances()[0].Balance

	conflictCases := []struct {
		name string
		mod  func(Intent) Intent
		fee  int
	}{
		{"different account", func(i Intent) Intent { i.Account = "aa-2"; return i }, 30},
		{"different paymaster", func(i Intent) Intent { i.Paymaster = "pm-2"; return i }, 30},
		{"different asset", func(i Intent) Intent { i.Asset = "eth"; return i }, 30},
		{"different amount", func(i Intent) Intent { i.Amount = 1600; return i }, 30},
		{"different nonce", func(i Intent) Intent { i.Nonce = 2; return i }, 30},
		{"different fee", func(i Intent) Intent { return i }, 20},
	}
	for _, tc := range conflictCases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.mod(base)
			results, err := l.Submit(Batch{FeeBps: tc.fee, Intents: []Intent{in}})
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if results[0].Status != StatusConflict {
				t.Fatalf("status = %q, want conflict", results[0].Status)
			}
		})
	}
	if got := l.Balances()[0].Balance; got != balanceAfterFirst {
		t.Fatalf("balance changed after conflicts: %d, want %d", got, balanceAfterFirst)
	}
	if len(l.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(l.Records()))
	}
}

func TestUnsuccessfulIdCanBeResubmitted(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 50}})
	l := openLedger(t, path)
	defer l.Close()
	in := intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending")
	first, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{in}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if first[0].Status != StatusInsufficient {
		t.Fatalf("first = %q, want insufficient", first[0].Status)
	}
	// Top up, then resubmit the same id with identical fields.
	l.st.Balances[0].Balance = 1000
	second, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{in}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if second[0].Status != StatusSettled {
		t.Fatalf("second = %q, want settled", second[0].Status)
	}
}

func TestSameBatchDuplicateIdFollowsRules(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	in := intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending")
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{in, in}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusSettled {
		t.Fatalf("first = %q", results[0].Status)
	}
	if results[1].Status != StatusDuplicate {
		t.Fatalf("second = %q, want duplicate", results[1].Status)
	}
	if len(l.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(l.Records()))
	}
}

func TestSameBatchDuplicateIdConflict(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	a := intent("pay-1", "aa-1", "pm-1", "usdc", 100, 1, "pending")
	b := intent("pay-1", "aa-1", "pm-1", "usdc", 200, 1, "pending")
	results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{a, b}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusSettled {
		t.Fatalf("first = %q", results[0].Status)
	}
	if results[1].Status != StatusConflict {
		t.Fatalf("second = %q, want conflict", results[1].Status)
	}
}

// ---- Persistence ----

func TestPersistenceAcrossReopen(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 10000}})
	l := openLedger(t, path)
	results, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 1500, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if results[0].Status != StatusSettled {
		t.Fatalf("status = %q", results[0].Status)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and continue.
	l2 := openLedger(t, path)
	defer l2.Close()
	if got := l2.Balances()[0].Balance; got != 8496 {
		t.Fatalf("balance after reopen = %d, want 8496", got)
	}
	records := l2.Records()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	r := records[0]
	if r.Intent.ID != "pay-1" || r.FeeBps != 30 || r.Charged != 1504 || r.Seq != 1 {
		t.Fatalf("record = %+v", r)
	}

	// Continue submitting on the reopened ledger.
	results2, err := l2.Submit(Batch{FeeBps: 0, Intents: []Intent{
		intent("pay-2", "aa-1", "pm-1", "usdc", 100, 2, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit2: %v", err)
	}
	if results2[0].Status != StatusSettled {
		t.Fatalf("pay-2 = %q", results2[0].Status)
	}
	if got := l2.Balances()[0].Balance; got != 8396 {
		t.Fatalf("balance = %d, want 8396", got)
	}
}

func TestOpenMissingLedger(t *testing.T) {
	path := tempLedger(t)
	_, err := Open(path)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open = %v, want ErrNotFound", err)
	}
}

func TestOpenCorruptLedger(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	// Corrupt the file.
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(path)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Open corrupt = %v, want ErrInvalid", err)
	}
}

func TestOpenCorruptLedgerDoesNotReset(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 1000}})
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Open(path)
	if err == nil {
		t.Fatalf("Open corrupt succeeded")
	}
	// File must still be corrupt, not reset to empty.
	data, _ := os.ReadFile(path)
	if string(data) == "" {
		t.Fatalf("corrupt ledger was reset to empty")
	}
}

// ---- Query ordering ----

func TestRecordsReturnedInSettlementOrder(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()
	ids := []string{"pay-3", "pay-1", "pay-2"}
	for i, id := range ids {
		results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
			intent(id, "aa-1", "pm-1", "usdc", 100, uint64(i+1), "pending"),
		}})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if results[0].Status != StatusSettled {
			t.Fatalf("%s = %q", id, results[0].Status)
		}
	}
	records := l.Records()
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	for i, r := range records {
		if r.Intent.ID != ids[i] {
			t.Fatalf("record[%d].ID = %q, want %q", i, r.Intent.ID, ids[i])
		}
		if r.Seq != int64(i+1) {
			t.Fatalf("record[%d].Seq = %d, want %d", i, r.Seq, i+1)
		}
	}
}

// ---- Concurrency ----

func TestConcurrentSubmitsSameLedger(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()

	const goroutines = 10
	const perGoroutine = 5
	var wg sync.WaitGroup
	statuses := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ids := make([]Intent, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				ids[i] = intent(
					fmt.Sprintf("g%d-pay-%d", g, i),
					"aa-1", "pm-1", "usdc", 100, uint64(g*100+i+1), "pending",
				)
			}
			results, err := l.Submit(Batch{FeeBps: 0, Intents: ids})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			s := make([]string, len(results))
			for i, r := range results {
				s[i] = r.Status
			}
			statuses[g] = s
		}(g)
	}
	wg.Wait()

	// Every item must be settled (enough balance for all).
	settled := 0
	for _, s := range statuses {
		for _, st := range s {
			if st == StatusSettled {
				settled++
			}
		}
	}
	if settled != goroutines*perGoroutine {
		t.Fatalf("settled = %d, want %d", settled, goroutines*perGoroutine)
	}
	if len(l.Records()) != goroutines*perGoroutine {
		t.Fatalf("records = %d, want %d", len(l.Records()), goroutines*perGoroutine)
	}
	if got := l.Balances()[0].Balance; got != 100000-int64(goroutines*perGoroutine*100) {
		t.Fatalf("balance = %d, want %d", got, 100000-goroutines*perGoroutine*100)
	}
}

func TestConcurrentSameIdOnlyOneSuccess(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})
	l := openLedger(t, path)
	defer l.Close()

	const goroutines = 20
	var wg sync.WaitGroup
	statuses := make([]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
				intent("same-id", "aa-1", "pm-1", "usdc", 100, 1, "pending"),
			}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			statuses[g] = results[0].Status
		}(g)
	}
	wg.Wait()

	settled, duplicate := 0, 0
	for _, s := range statuses {
		switch s {
		case StatusSettled:
			settled++
		case StatusDuplicate:
			duplicate++
		}
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}
	if duplicate != goroutines-1 {
		t.Fatalf("duplicate = %d, want %d", duplicate, goroutines-1)
	}
	if len(l.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(l.Records()))
	}
}

func TestConcurrentCompetingBalanceOnlyOneSucceeds(t *testing.T) {
	path := tempLedger(t)
	// Only enough for one payment of 100.
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100}})
	l := openLedger(t, path)
	defer l.Close()

	const goroutines = 10
	var wg sync.WaitGroup
	statuses := make([]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
				intent(fmt.Sprintf("pay-%d", g), "aa-1", "pm-1", "usdc", 100, uint64(g+1), "pending"),
			}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			statuses[g] = results[0].Status
		}(g)
	}
	wg.Wait()

	settled := 0
	for _, s := range statuses {
		if s == StatusSettled {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}
	if len(l.Records()) != 1 {
		t.Fatalf("records = %d, want 1", len(l.Records()))
	}
	if got := l.Balances()[0].Balance; got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestConcurrentSeparateOpensSamePath(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 100000}})

	const goroutines = 8
	var wg sync.WaitGroup
	statuses := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			l, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer l.Close()
			results, err := l.Submit(Batch{FeeBps: 0, Intents: []Intent{
				intent(fmt.Sprintf("pay-%d", g), "aa-1", "pm-1", "usdc", 100, uint64(g+1), "pending"),
			}})
			if err != nil {
				t.Errorf("Submit: %v", err)
				return
			}
			statuses[g] = []string{results[0].Status}
		}(g)
	}
	wg.Wait()

	settled := 0
	for _, s := range statuses {
		if len(s) > 0 && s[0] == StatusSettled {
			settled++
		}
	}
	if settled != goroutines {
		t.Fatalf("settled = %d, want %d", settled, goroutines)
	}

	// Reopen and verify final state.
	l := openLedger(t, path)
	defer l.Close()
	if len(l.Records()) != goroutines {
		t.Fatalf("records = %d, want %d", len(l.Records()), goroutines)
	}
	if got := l.Balances()[0].Balance; got != 100000-int64(goroutines*100) {
		t.Fatalf("balance = %d, want %d", got, 100000-goroutines*100)
	}
}

// ---- JSON round-trip ----

func TestStateJSONRoundTrip(t *testing.T) {
	path := tempLedger(t)
	initBalances(t, path, []Balance{{Account: "aa-1", Asset: "usdc", Balance: 10000}})
	l := openLedger(t, path)
	_, err := l.Submit(Batch{FeeBps: 30, Intents: []Intent{
		intent("pay-1", "aa-1", "pm-1", "usdc", 1500, 1, "pending"),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	l.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if st.Version != 1 || len(st.Balances) != 1 || len(st.Records) != 1 {
		t.Fatalf("state = %+v", st)
	}
}

// ---- Existing calls still work ----

func TestExecuteAndReconcileStillWork(t *testing.T) {
	intents := []Intent{
		{ID: "pay-1", Account: "aa-1", Paymaster: "pm-1", Amount: 1500, Asset: "usdc", Nonce: 1, State: "pending"},
		{ID: "pay-2", Account: "aa-1", Paymaster: "pm-1", Amount: 900, Asset: "usdc", Nonce: 2, State: "pending"},
	}
	spent := map[string]bool{}
	var settlements []Settlement
	for _, in := range intents {
		settlements = append(settlements, Execute(in, 30, spent, 2000))
	}
	// pay-1: 1500 + 45 = 1545 <= 2000 settled; pay-2: 900 + 2 = 902 <= 2000 settled.
	if settlements[0].Status != "settled" || settlements[1].Status != "settled" {
		t.Fatalf("settlements = %+v", settlements)
	}
	gaps := Reconcile(intents, settlements)
	if len(gaps) != 0 {
		t.Fatalf("gaps = %v, want empty", gaps)
	}

	// Insufficient balance still reports failed.
	spent2 := map[string]bool{}
	failed := Execute(intents[0], 30, spent2, 100)
	if failed.Status != "failed" {
		t.Fatalf("failed status = %q, want failed", failed.Status)
	}
}
