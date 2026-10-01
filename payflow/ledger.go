// Package payflow implements payment intent settlement and reconciliation.
package payflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"
)

// Per-item result statuses.
const (
	StatusSettled      = "settled"
	StatusDuplicate    = "duplicate"
	StatusConflict     = "conflict"
	StatusStateError   = "state_error"
	StatusInsufficient = "insufficient"
	StatusRejected     = "rejected"
	StatusStorageError = "storage_error"
)

// Ledger errors.
var (
	ErrNotFound = errors.New("ledger not found")
	ErrExists   = errors.New("ledger already exists")
	ErrInvalid  = errors.New("invalid ledger data")
)

// State is the persisted ledger state.
type State struct {
	Version  int       `json:"version"`
	Balances []Balance `json:"balances"`
	Records  []Record  `json:"records"`
}

// Balance is an account-asset balance.
type Balance struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Balance int64  `json:"balance"`
}

// Record is a settled payment, traceable to the original intent and fee.
type Record struct {
	Intent  Intent `json:"intent"`
	FeeBps  int    `json:"fee_bps"`
	Charged int64  `json:"charged"`
	Ref     string `json:"ref"`
	Seq     int64  `json:"seq"`
}

// Result is the per-item outcome, aligned with the input order.
type Result struct {
	Intent  string `json:"intent"`
	Status  string `json:"status"`
	Charged int64  `json:"charged,omitempty"`
	FeeBps  int    `json:"fee_bps,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Batch is a JSON payment batch carrying a fixed fee rate.
type Batch struct {
	FeeBps  int      `json:"fee_bps"`
	Intents []Intent `json:"intents"`
}

// Ledger is an open local ledger. A Ledger is safe for concurrent use by
// multiple goroutines; the in-process mutex serializes them while the
// flock serializes separate opens of the same path.
type Ledger struct {
	path string
	lock *os.File
	mu   sync.Mutex
	st   State
}

func lockPath(path string) string { return path + ".lock" }

// Init creates a new ledger at path with the given balances.
// It validates everything before creating anything and refuses to
// overwrite an existing ledger.
func Init(path string, balances []Balance) error {
	if err := validateBalances(balances); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	} else if !os.IsNotExist(err) {
		return err
	}
	st := State{Version: 1, Balances: cloneBalances(balances), Records: []Record{}}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// Open opens an existing ledger, taking an exclusive lock for its lifetime.
// A missing ledger or a corrupt one is reported, never silently reset.
func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(lockPath(path), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		f.Close()
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := validateState(st); err != nil {
		f.Close()
		return nil, err
	}
	return &Ledger{path: path, lock: f, st: st}, nil
}

// Close releases the ledger lock.
func (l *Ledger) Close() error {
	if l.lock == nil {
		return nil
	}
	err := syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
	cerr := l.lock.Close()
	l.lock = nil
	if err != nil {
		return err
	}
	return cerr
}

// Submit applies a batch, returning one result per intent in input order.
// Each item is settled independently and saved atomically; a save failure
// preserves that item's pre-execution state while earlier items stand.
func (l *Ledger) Submit(batch Batch) ([]Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	results := make([]Result, 0, len(batch.Intents))
	for _, in := range batch.Intents {
		results = append(results, l.process(in, batch.FeeBps))
	}
	return results, nil
}

// Balances returns the current account-asset balances.
func (l *Ledger) Balances() []Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneBalances(l.st.Balances)
}

// Records returns settled records in settlement success order.
func (l *Ledger) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, len(l.st.Records))
	copy(out, l.st.Records)
	return out
}

func (l *Ledger) process(in Intent, feeBps int) Result {
	// 1. Parameter validation.
	if in.ID == "" || in.Account == "" || in.Asset == "" || in.Paymaster == "" {
		return Result{Intent: in.ID, Status: StatusRejected, Reason: "empty required field"}
	}
	if in.Amount <= 0 {
		return Result{Intent: in.ID, Status: StatusRejected, Reason: "amount must be a positive int64"}
	}
	if feeBps < 0 || feeBps > 10000 {
		return Result{Intent: in.ID, Status: StatusRejected, Reason: "fee rate out of range [0,10000] bps"}
	}
	feeProduct, ok := mul64(in.Amount, int64(feeBps))
	if !ok {
		return Result{Intent: in.ID, Status: StatusRejected, Reason: "fee amount overflows int64"}
	}
	feeAmount := feeProduct / 10000
	total, ok := add64(in.Amount, feeAmount)
	if !ok {
		return Result{Intent: in.ID, Status: StatusRejected, Reason: "deduction total overflows int64"}
	}

	// 2. State check: non-pending is reported on its own.
	if in.State != "pending" {
		return Result{Intent: in.ID, Status: StatusStateError, Reason: "intent is not pending"}
	}

	// 3. Dedup: a settled id with identical fields replays; otherwise conflict.
	if rec, found := l.findRecord(in.ID); found {
		if rec.matches(in, feeBps) {
			return Result{
				Intent:  in.ID,
				Status:  StatusDuplicate,
				Charged: rec.Charged,
				FeeBps:  rec.FeeBps,
				Ref:     rec.Ref,
				Reason:  "duplicate of settled intent",
			}
		}
		return Result{Intent: in.ID, Status: StatusConflict, Reason: "intent id conflicts with a settled payment"}
	}

	// 4. Balance check: a missing account-asset is treated as insufficient.
	idx := l.findBalance(in.Account, in.Asset)
	if idx < 0 || l.st.Balances[idx].Balance < total {
		return Result{Intent: in.ID, Status: StatusInsufficient, Reason: "insufficient balance for amount plus fee"}
	}

	// 5. Settle: mutate in memory, then save; roll back on save failure.
	l.st.Balances[idx].Balance -= total
	rec := Record{
		Intent:  in,
		FeeBps:  feeBps,
		Charged: total,
		Ref:     "settle:" + in.ID,
		Seq:     l.nextSeq(),
	}
	l.st.Records = append(l.st.Records, rec)
	if err := l.save(); err != nil {
		l.st.Balances[idx].Balance += total
		l.st.Records = l.st.Records[:len(l.st.Records)-1]
		return Result{Intent: in.ID, Status: StatusStorageError, Reason: err.Error()}
	}
	return Result{Intent: in.ID, Status: StatusSettled, Charged: total, FeeBps: feeBps, Ref: rec.Ref}
}

func (l *Ledger) findRecord(id string) (Record, bool) {
	for _, r := range l.st.Records {
		if r.Intent.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

func (r Record) matches(in Intent, feeBps int) bool {
	return r.FeeBps == feeBps &&
		r.Intent.Account == in.Account &&
		r.Intent.Paymaster == in.Paymaster &&
		r.Intent.Asset == in.Asset &&
		r.Intent.Amount == in.Amount &&
		r.Intent.Nonce == in.Nonce
}

func (l *Ledger) findBalance(account, asset string) int {
	for i, b := range l.st.Balances {
		if b.Account == account && b.Asset == asset {
			return i
		}
	}
	return -1
}

func (l *Ledger) nextSeq() int64 {
	var max int64
	for _, r := range l.st.Records {
		if r.Seq > max {
			max = r.Seq
		}
	}
	return max + 1
}

func (l *Ledger) save() error {
	data, err := json.MarshalIndent(l.st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(l.path, data)
}

func validateBalances(balances []Balance) error {
	seen := make(map[string]bool)
	for _, b := range balances {
		if b.Account == "" || b.Asset == "" {
			return fmt.Errorf("%w: empty account or asset", ErrInvalid)
		}
		if b.Balance < 0 {
			return fmt.Errorf("%w: negative balance for %s/%s", ErrInvalid, b.Account, b.Asset)
		}
		key := b.Account + "\x00" + b.Asset
		if seen[key] {
			return fmt.Errorf("%w: duplicate balance for %s/%s", ErrInvalid, b.Account, b.Asset)
		}
		seen[key] = true
	}
	return nil
}

func validateState(st State) error {
	if st.Version != 1 {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalid, st.Version)
	}
	if err := validateBalances(st.Balances); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, r := range st.Records {
		if r.Intent.ID == "" || r.Intent.Account == "" || r.Intent.Asset == "" || r.Intent.Paymaster == "" {
			return fmt.Errorf("%w: record with empty required field", ErrInvalid)
		}
		if r.Intent.Amount <= 0 {
			return fmt.Errorf("%w: record with non-positive amount", ErrInvalid)
		}
		if r.FeeBps < 0 || r.FeeBps > 10000 {
			return fmt.Errorf("%w: record with invalid fee rate", ErrInvalid)
		}
		if r.Charged < 0 {
			return fmt.Errorf("%w: record with negative charged amount", ErrInvalid)
		}
		if seen[r.Intent.ID] {
			return fmt.Errorf("%w: duplicate settled id %q", ErrInvalid, r.Intent.ID)
		}
		seen[r.Intent.ID] = true
	}
	return nil
}

func cloneBalances(in []Balance) []Balance {
	out := make([]Balance, len(in))
	copy(out, in)
	return out
}

// writeAtomic writes data to a temp file in the same directory, fsyncs it,
// and renames it over the target so a crash leaves either the old or the
// new complete state, never a partial one.
func writeAtomic(path string, data []byte) error {
	dir := dirOf(path)
	f, err := os.CreateTemp(dir, ".payflow-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	keep = true
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}

// mul64 multiplies two non-negative int64 values, reporting overflow.
func mul64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	c := a * b
	if c/a != b {
		return 0, false
	}
	return c, true
}

// add64 adds two non-negative int64 values, reporting overflow.
func add64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
