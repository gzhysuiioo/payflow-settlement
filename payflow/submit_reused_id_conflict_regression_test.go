package payflow

import (
	"reflect"
	"testing"
)

// 本文件为本地账本 Ledger.Submit 锁定“同一批次内刚成功的付款编号被后续请求
// 再次使用”时的判断，重点保护：
//
//  1. 批次开始时编号还没有成功记录：首项按正常费率扣款并留下记录，同批次
//     后项随即复用该编号时必须看到这条刚成功的记录。
//  2. 逐项结果严格对应提交顺序；批次本身合法时 Submit 正常返回逐项结果，
//     逐项 conflict 不导致整批失败。
//  3. 后项参数合法、只改变账户、付款方、资产、金额或 nonce 中任意一个字段时，
//     一律返回 conflict，不携带成功结算记录、不再次扣款、不分配新序号。
//     改账户/资产的变体使用余额充足的组合，确保 conflict 确实来自编号已被
//     占用（重复/冲突判断先于限额与余额），而不是被其他拒绝条件掩盖。
//  4. 同编号请求的全部付款字段（账户、付款方、资产、金额、nonce）与费率都与
//     首项相同时返回 duplicate，附带首项的完整原记录，不再次扣款、不占序号。
//  5. conflict 与 duplicate 都不消耗本批次扣款上限：在带限额的连续付款中，
//     已成功编号的冲突判断优先于剩余额度与余额判断，冲突/重复项不占额度，
//     后续新付款仍可恰好用完剩余余额与额度。
//
// 基础场景与 submit_result_isolation_test.go 一致：aa/usdc 付款 1500，
// 费率 30 基点，付款方 pm-1、nonce 7，手续费 4、扣款 1504。
//
// 公开调用方式（FeeBatch/PaymentIntent/ItemResult/Record）与付款数值规则
// 均不在本文件变更。

// reuseConflictPayment 在首项付款 baseScenarioPayment("p1") 的基础上只改变
// 一个付款字段，返回同编号、参数合法的冲突请求；其余付款内容（含费率语境）
// 与首项完全一致。
func reuseConflictPayment(field string) PaymentIntent {
	it := baseScenarioPayment("p1")
	switch field {
	case "account":
		it.Account = "bb"
	case "paymaster":
		it.Paymaster = "pm-2"
	case "asset":
		it.Asset = "eth"
	case "amount":
		*it.Amount = 1501
	case "nonce":
		it.Nonce = 8
	default:
		panic("unknown conflict field: " + field)
	}
	return it
}

// TestSubmitReusedSettledIDConflictPerField 逐字段保护同批次内的编号冲突判断：
// 首项正常 settled 后，只改一个付款字段的同编号合法后项必须 conflict、
// 不带成功记录；首项记录的账户、金额、费用与成功序号保持原值，相关组合的
// 余额都不因冲突项变化。
func TestSubmitReusedSettledIDConflictPerField(t *testing.T) {
	const (
		startAAUSDC = int64(100000)
		startBBUSDC = int64(100000)
		startAAETH  = int64(100000)
	)
	fields := []string{"account", "paymaster", "asset", "amount", "nonce"}

	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			path := newTestLedger(t, []BalanceInit{
				{Account: "aa", Asset: "usdc", Balance: startAAUSDC},
				{Account: "bb", Asset: "usdc", Balance: startBBUSDC},
				{Account: "aa", Asset: "eth", Balance: startAAETH},
			})
			l := openOrFail(t, path)

			variant := reuseConflictPayment(field)

			// 变体本身的数值前提：改付款方或 nonce 时手续费与扣款总额与首项
			// 完全相同（fee=4、charged=1504），但仍属于不同请求；改金额为 1501
			// 时手续费同样是 4、扣款 1505；改账户/资产时扣款仍是 1504，
			// 且目标组合余额充足。下列变体若按新付款处理都能通过余额检查，
			// 因此 conflict 只能来自编号已被首项占用。
			fee, total, ok := chargeFor(*variant.Amount, 30)
			if !ok {
				t.Fatalf("variant must be numerically valid: amount=%d", *variant.Amount)
			}
			switch field {
			case "paymaster", "nonce", "account", "asset":
				if fee != 4 || total != 1504 {
					t.Fatalf("%s variant must cost exactly as much as the first item, got fee=%d charged=%d",
						field, fee, total)
				}
			case "amount":
				if fee != 4 || total != 1505 {
					t.Fatalf("amount variant fee=%d charged=%d want 4/1505", fee, total)
				}
			}

			// 同一批次：首项 settled，字段有别的同编号后项 conflict。
			res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{
				baseScenarioPayment("p1"),
				variant,
			}})
			if err != nil {
				t.Fatalf("a legal batch containing a conflict must return per-item results: %v", err)
			}
			if got := statuses(res); !reflect.DeepEqual(got, []string{StatusSettled, StatusConflict}) {
				t.Fatalf("statuses=%v want [settled conflict]", got)
			}

			wantP1 := baseScenarioRecord("p1", 1)

			// 首项按正常费率扣款并留下完整原记录，成功序号为 1。
			first := res.Results[0]
			if first.ID != "p1" || first.Record == nil || *first.Record != wantP1 {
				t.Fatalf("first item must settle with its original record: %+v want %+v",
					first, wantP1)
			}

			// 后项 conflict：逐项对应自己的提交位置与编号，只带状态与说明，
			// 不携带任何成功结算记录。
			cf := res.Results[1]
			if cf.ID != "p1" || cf.Status != StatusConflict || cf.Reason != reasonConflict {
				t.Fatalf("second item must be a conflict on the reused id: %+v", cf)
			}
			if cf.Record != nil {
				t.Fatalf("conflict item must not carry a settlement record: %+v", cf.Record)
			}

			// 首项记录的账户、金额、费用与序号保持原值；成功历史只有首项一笔。
			snap := mustQuery(t, l)
			if !reflect.DeepEqual(snap.Settlements, []Record{wantP1}) {
				t.Fatalf("settlements:\n got %+v\nwant %+v", snap.Settlements, wantP1)
			}

			// 只有首项扣过款：aa/usdc 减少 1504；冲突项可能指向的其他组合
			// （bb/usdc、aa/eth）余额一律保持原值。
			if bal, _ := l.Balance("aa", "usdc"); bal != startAAUSDC-1504 {
				t.Fatalf("aa/usdc balance=%d want %d", bal, startAAUSDC-1504)
			}
			if bal, _ := l.Balance("bb", "usdc"); bal != startBBUSDC {
				t.Fatalf("bb/usdc changed by the conflict item: %d want %d", bal, startBBUSDC)
			}
			if bal, _ := l.Balance("aa", "eth"); bal != startAAETH {
				t.Fatalf("aa/eth changed by the conflict item: %d want %d", bal, startAAETH)
			}

			// 编号仍为首项原请求占用：原样再提一次得到 duplicate 与首项完整
			// 原记录，依旧不扣款——证明 conflict 项既没有顶替原记录，也没有
			// 释放或改写该编号。
			dup := mustSettle(t, l, 30, baseScenarioPayment("p1"))
			if dup.Status != StatusDuplicate || dup.Record == nil || *dup.Record != wantP1 {
				t.Fatalf("original retry must duplicate the first record: %+v", dup)
			}
			if bal, _ := l.Balance("aa", "usdc"); bal != startAAUSDC-1504 {
				t.Fatalf("duplicate after conflict charged again: bal=%d", bal)
			}
			if len(mustQuery(t, l).Settlements) != 1 {
				t.Fatalf("conflict and duplicate must not add settlement records")
			}
		})
	}
}

// TestSubmitReusedSettledIDIdenticalIsDuplicateInSameBatch 保护同批次内的
// 幂等重试：同编号的付款内容（账户、付款方、资产、金额、nonce）与费率完全
// 相同时，后项返回 duplicate 并附带首项的完整原记录，不再次扣款，也不分配
// 新的成功序号；两项结果各自持有独立的记录副本。
func TestSubmitReusedSettledIDIdenticalIsDuplicateInSameBatch(t *testing.T) {
	const startAA int64 = 100000
	path := newTestLedger(t, []BalanceInit{{Account: "aa", Asset: "usdc", Balance: startAA}})
	l := openOrFail(t, path)

	req := baseScenarioPayment("p1")
	res, err := l.Submit(FeeBatch{FeeBps: 30, Intents: []PaymentIntent{req, req}})
	if err != nil {
		t.Fatalf("identical in-batch retry must return per-item results: %v", err)
	}
	if got := statuses(res); !reflect.DeepEqual(got, []string{StatusSettled, StatusDuplicate}) {
		t.Fatalf("statuses=%v want [settled duplicate]", got)
	}

	wantP1 := baseScenarioRecord("p1", 1)
	first, dup := res.Results[0], res.Results[1]
	if first.Record == nil || *first.Record != wantP1 {
		t.Fatalf("first item record: %+v want %+v", first.Record, wantP1)
	}
	// duplicate 附带首项的完整原记录（含原账户、付款方、资产、金额、nonce、
	// 费率、手续费、扣款总额与成功序号 1），而不是新记录。
	if dup.ID != "p1" || dup.Status != StatusDuplicate || dup.Reason != reasonDuplicate {
		t.Fatalf("second item must duplicate the first: %+v", dup)
	}
	if dup.Record == nil || *dup.Record != wantP1 {
		t.Fatalf("duplicate must carry the full original record: %+v want %+v",
			dup.Record, wantP1)
	}
	if dup.Record == first.Record {
		t.Fatal("settled and duplicate items must not share one *Record allocation")
	}

	// 整批只扣一次款；成功历史只有一笔，序号为 1，没有新序号分配给重复项。
	if bal, _ := l.Balance("aa", "usdc"); bal != startAA-1504 {
		t.Fatalf("duplicate charged again: bal=%d want %d", bal, startAA-1504)
	}
	if snap := mustQuery(t, l); !reflect.DeepEqual(snap.Settlements, []Record{wantP1}) {
		t.Fatalf("settlements:\n got %+v\nwant %+v", snap.Settlements, wantP1)
	}

	// 再次原样提交仍是 duplicate、仍是原记录，余额不变。
	again := mustSettle(t, l, 30, req)
	if again.Status != StatusDuplicate || again.Record == nil || *again.Record != wantP1 {
		t.Fatalf("third identical retry: %+v", again)
	}
	if bal, _ := l.Balance("aa", "usdc"); bal != startAA-1504 {
		t.Fatalf("third retry charged: bal=%d want %d", bal, startAA-1504)
	}
}

// TestSubmitReusedIDConflictAndDuplicateDoNotConsumeBatchQuota 是任务给定的
// 连续付款场景：尚无付款历史的账本，aa/usdc 余额与本批次扣款上限都是 1604，
// 费率 30 基点。
//
//	1) p1 付款 1500：手续费 4、扣款 1504，settled，seq=1，余 100；
//	2) 同编号金额 1501 的合法请求：charged 1505，剩余额度与余额都只剩 100，
//	   本应超限（且余额不足），但已成功编号的冲突判断优先，必须 conflict；
//	3) 首项原样请求：duplicate，携带首项完整原记录；
//	4) 新编号 p2 付款 100：30bps 下手续费为 0、扣款 100，恰好用完剩余
//	   100 的余额与额度，settled，seq=2。
//
// 四项依次 settled、conflict、duplicate、settled；成功历史只留下两笔付款，
// 序号为 1、2；aa/usdc 归零，其他账户和资产的余额保持原值。最后一项能够
// 成功同时证明 conflict 与 duplicate 都不占本批次额度（也不扣余额）。
func TestSubmitReusedIDConflictAndDuplicateDoNotConsumeBatchQuota(t *testing.T) {
	const startAA int64 = 1604
	path := newTestLedger(t, []BalanceInit{
		{Account: "aa", Asset: "usdc", Balance: startAA},
		{Account: "bb", Asset: "usdc", Balance: 500}, // 哨兵：其他账户余额不变
		{Account: "aa", Asset: "eth", Balance: 50},   // 哨兵：其他资产余额不变
	})
	l := openOrFail(t, path)

	// 明确数值前提：1500@30bps => fee 4 / charged 1504；100@30bps => fee 0。
	if fee := feeFor(1500, 30); fee != 4 {
		t.Fatalf("feeFor(1500,30)=%d want 4", fee)
	}
	if fee := feeFor(100, 30); fee != 0 {
		t.Fatalf("feeFor(100,30)=%d want 0", fee)
	}
	if _, total, ok := chargeFor(1501, 30); !ok || total != 1505 {
		t.Fatalf("1501@30bps must be a valid request charging 1505, got total=%d ok=%v", total, ok)
	}

	res, err := l.Submit(FeeBatch{
		FeeBps: 30,
		// 本批次 aa/usdc 扣款上限恰为 1604，与初始余额相同。
		Limits: []ChargeLimit{limit("aa", "usdc", startAA)},
		Intents: []PaymentIntent{
			baseScenarioPayment("p1"),        // 1) 首次成功：charged 1504
			reuseConflictPayment("amount"),  // 2) 同编号 1501：编号冲突
			baseScenarioPayment("p1"),       // 3) 原样重试：幂等重复
			intent("p2", "aa", "usdc", 100), // 4) 新编号 100：fee 0，用尽余 100
		},
	})
	if err != nil {
		t.Fatalf("the batch is legal and must return per-item results: %v", err)
	}
	wantStatus := []string{StatusSettled, StatusConflict, StatusDuplicate, StatusSettled}
	if got := statuses(res); !reflect.DeepEqual(got, wantStatus) {
		t.Fatalf("statuses=%v want %v", got, wantStatus)
	}
	if len(res.Results) != 4 {
		t.Fatalf("want 4 per-item results in submission order, got %d", len(res.Results))
	}

	wantP1 := baseScenarioRecord("p1", 1)
	wantP2 := Record{
		ID: "p2", Account: "aa", Paymaster: "pm", Asset: "usdc",
		Amount: 100, Nonce: 1, FeeBps: 30, Fee: 0, Charged: 100, Seq: 2,
	}

	// 1) 首项：账户、金额、费用、扣款总额与成功序号均为首项原值。
	r1 := res.Results[0]
	if r1.ID != "p1" || r1.Record == nil || *r1.Record != wantP1 {
		t.Fatalf("item 1 must settle with the original record: %+v want %+v", r1, wantP1)
	}

	// 2) 冲突项：不带成功记录；剩余额度/余额只有 100，此项收 1505 本会
	//    报告 limit_exceeded，拿到 conflict 即锁定“已成功编号判断优先于
	//    剩余额度与余额判断”。
	r2 := res.Results[1]
	if r2.ID != "p1" || r2.Status != StatusConflict || r2.Reason != reasonConflict || r2.Record != nil {
		t.Fatalf("item 2 must be conflict without a record: %+v", r2)
	}

	// 3) 重复项：附带首项完整原记录（序号仍是 1、charged 1504），与首项
	//    结果是独立分配。
	r3 := res.Results[2]
	if r3.ID != "p1" || r3.Status != StatusDuplicate || r3.Reason != reasonDuplicate {
		t.Fatalf("item 3 must be duplicate: %+v", r3)
	}
	if r3.Record == nil || *r3.Record != wantP1 {
		t.Fatalf("item 3 must carry the full first record: %+v want %+v", r3.Record, wantP1)
	}
	if r3.Record == r1.Record {
		t.Fatal("settled and duplicate items must not share one *Record allocation")
	}

	// 4) 新编号付款：手续费为零、扣款 100、seq=2——只有 conflict/duplicate
	//    都不占额度时，这项才能恰好用完剩余 100 的额度与余额而成功。
	r4 := res.Results[3]
	if r4.ID != "p2" || r4.Record == nil || *r4.Record != wantP2 {
		t.Fatalf("item 4 must settle fee-free with seq 2: %+v want %+v", r4, wantP2)
	}

	// 成功历史只留下两笔付款，序号 1、2；首项记录保持原值。
	snap := mustQuery(t, l)
	if !reflect.DeepEqual(snap.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("settlements:\n got %+v\nwant %+v / %+v", snap.Settlements, wantP1, wantP2)
	}

	// aa/usdc 恰好用尽（1504 + 100 = 1604）；其他账户和资产的余额保持原值。
	if bal, _ := l.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("aa/usdc balance=%d want 0", bal)
	}
	if bal, _ := l.Balance("bb", "usdc"); bal != 500 {
		t.Fatalf("bb/usdc balance=%d want 500 (must stay untouched)", bal)
	}
	if bal, _ := l.Balance("aa", "eth"); bal != 50 {
		t.Fatalf("aa/eth balance=%d want 50 (must stay untouched)", bal)
	}

	// 关闭重开：磁盘上同样只有两笔成功记录、aa/usdc 归零，冲突与重复从未落盘。
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l2 := openOrFail(t, path)
	snap2 := mustQuery(t, l2)
	if !reflect.DeepEqual(snap2.Settlements, []Record{wantP1, wantP2}) {
		t.Fatalf("after reopen settlements:\n got %+v\nwant %+v / %+v",
			snap2.Settlements, wantP1, wantP2)
	}
	if bal, _ := l2.Balance("aa", "usdc"); bal != 0 {
		t.Fatalf("after reopen aa/usdc=%d want 0", bal)
	}
}
