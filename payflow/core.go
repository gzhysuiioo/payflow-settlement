// Package payflow implements payment intent settlement and reconciliation.
package payflow

import "sort"

// Intent is one payment request from an account abstraction wallet.
type Intent struct {
	ID        string
	Account   string
	Paymaster string
	Amount    int64
	Asset     string
	Nonce     uint64
	State     string
}

// Settlement is the ledger effect of executing an intent.
type Settlement struct {
	Intent  string
	Charged int64
	FeeBps  int
	Ref     string
	Status  string
	Reason  string
}

// Execute applies fee policy and enforces per-intent idempotency.
func Execute(intent Intent, feeBps int, spent map[string]bool, balance int64) Settlement {
	if intent.State != "pending" {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: "intent is not pending"}
	}
	if spent[intent.ID] {
		return Settlement{Intent: intent.ID, Status: "rejected", Reason: "duplicate settlement attempt"}
	}
	fee := intent.Amount * int64(feeBps) / 10000
	total := intent.Amount + fee
	if total > balance {
		return Settlement{Intent: intent.ID, Status: "failed", Reason: "insufficient balance for amount plus fee"}
	}
	spent[intent.ID] = true
	return Settlement{Intent: intent.ID, Charged: total, FeeBps: feeBps, Ref: "settle:" + intent.ID, Status: "settled"}
}

// Reconcile reports intents with no matching settlement, in stable order.
func Reconcile(intents []Intent, settlements []Settlement) []string {
	seen := map[string]bool{}
	for _, settlement := range settlements {
		if settlement.Status == "settled" {
			seen[settlement.Intent] = true
		}
	}
	var gaps []string
	for _, intent := range intents {
		if !seen[intent.ID] {
			gaps = append(gaps, intent.ID)
		}
	}
	sort.Strings(gaps)
	return gaps
}
