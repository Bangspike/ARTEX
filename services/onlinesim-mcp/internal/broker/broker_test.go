package broker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Broker, *MockProvider, *time.Time) {
	t.Helper()
	s, err := OpenStore(t.TempDir(), "mock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p := NewMockProvider()
	now := time.Now()
	p.now = func() time.Time { return now }
	b, err := New(p, s, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	b.now = p.now
	return b, p, &now
}
func reserve(t *testing.T, b *Broker, owner, service, key string) View {
	t.Helper()
	v, err := b.Reserve(context.Background(), owner, ReserveInput{Service: service, IdempotencyKey: key, MaxPrice: "0.20"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func TestLifecycleAndNextMessage(t *testing.T) {
	b, p, now := fixture(t)
	v := reserve(t, b, "task-1", "demo", "attempt-1")
	if v.Status != "waiting" || v.Number == "" {
		t.Fatalf("unexpected reservation: %+v", v)
	}
	*now = now.Add(3 * time.Second)
	b.Tick(context.Background())
	v, _ = b.Status("task-1", v.OrderID)
	if v.Code != "123456" || v.Status != "code_received" {
		t.Fatalf("OTP was not delivered: %+v", v)
	}
	v, err := b.Next(context.Background(), "task-1", v.OrderID)
	if err != nil || v.Code != "" {
		t.Fatalf("advance: %+v %v", v, err)
	}
	b.Tick(context.Background())
	v, _ = b.Status("task-1", v.OrderID)
	if v.Code != "123457" {
		t.Fatalf("next SMS: %+v", v)
	}
	v, err = b.Close("task-1", v.OrderID)
	if err != nil || v.Status != "close_pending" || v.Code != "" {
		t.Fatalf("close: %+v %v", v, err)
	}
	b.Tick(context.Background())
	v, _ = b.Status("task-1", v.OrderID)
	if v.Status != "closed" {
		t.Fatalf("cleanup: %+v", v)
	}
	if !p.orders[b.store.state.Orders[v.OrderID].ProviderID].closed {
		t.Fatal("provider left open")
	}
	replay := reserve(t, b, "task-1", "demo", "attempt-1")
	if replay.OrderID != v.OrderID || replay.Status != "closed" {
		t.Fatal("closed attempt purchased again")
	}
}
func TestConcurrentReserveIsIdempotent(t *testing.T) {
	b, p, _ := fixture(t)
	var wg sync.WaitGroup
	ids := make(chan string, 32)
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			v, e := b.Reserve(context.Background(), "task-1", ReserveInput{Service: "demo", IdempotencyKey: "same", MaxPrice: "0.20"})
			if e != nil {
				errs <- e
			}
			ids <- v.OrderID
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("multiple orders for one key")
		}
	}
	if p.seq != 1 {
		t.Fatalf("purchased %d times", p.seq)
	}
	_, err := b.Reserve(context.Background(), "task-1", ReserveInput{Service: "late_sms", IdempotencyKey: "same", MaxPrice: "0.20"})
	if errorCode(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal(err)
	}
}
func TestTaskIsolation(t *testing.T) {
	b, _, _ := fixture(t)
	v := reserve(t, b, "task-a", "demo", "same")
	for _, operation := range []func() error{func() error { _, e := b.Status("task-b", v.OrderID); return e }, func() error { _, e := b.Close("task-b", v.OrderID); return e }, func() error { _, e := b.Next(context.Background(), "task-b", v.OrderID); return e }} {
		if errorCode(operation()) != "ORDER_NOT_FOUND" {
			t.Fatal("foreign order accessible")
		}
	}
	other := reserve(t, b, "task-b", "demo", "same")
	if other.OrderID == v.OrderID {
		t.Fatal("keys not scoped to task")
	}
}
func TestLimitsBeforePurchase(t *testing.T) {
	for _, tc := range []struct {
		name, service, price, code string
		budget                     Amount
	}{{"stock", "no_stock", "0.20", "NO_NUMBER", 5000000}, {"price", "demo", "0.09", "PRICE_LIMIT_EXCEEDED", 5000000}, {"budget", "demo", "0.20", "BUDGET_LIMIT_EXCEEDED", 90000}, {"service", "unknown", "0.20", "SERVICE_NOT_AVAILABLE", 5000000}} {
		t.Run(tc.name, func(t *testing.T) {
			b, p, _ := fixture(t)
			b.config.TaskBudget = tc.budget
			_, e := b.Reserve(context.Background(), "a", ReserveInput{Service: tc.service, IdempotencyKey: "key", MaxPrice: tc.price})
			if errorCode(e) != tc.code {
				t.Fatalf("%v", e)
			}
			if p.seq != 0 {
				t.Fatal("bought before checking limits")
			}
		})
	}
	b, p, _ := fixture(t)
	reserve(t, b, "a", "demo", "one")
	reserve(t, b, "a", "demo", "two")
	_, e := b.Reserve(context.Background(), "a", ReserveInput{Service: "demo", IdempotencyKey: "three", MaxPrice: "0.20"})
	if errorCode(e) != "ORDER_LIMIT_EXCEEDED" || p.seq != 2 {
		t.Fatal("order limit not enforced")
	}
}
func TestUnknownPurchaseNeverRetriesAndOperatorRecovery(t *testing.T) {
	b, p, _ := fixture(t)
	v := reserve(t, b, "a", "purchase_timeout", "key")
	if v.Status != "purchase_unknown" {
		t.Fatal(v)
	}
	replay := reserve(t, b, "a", "purchase_timeout", "key")
	if replay.OrderID != v.OrderID || p.seq != 1 {
		t.Fatal("retried ambiguous buy")
	}
	_, err := b.Reserve(context.Background(), "a", ReserveInput{Service: "demo", IdempotencyKey: "new", MaxPrice: "0.20"})
	if errorCode(err) != "PURCHASE_RECONCILIATION_REQUIRED" {
		t.Fatal(err)
	}
	recovered, err := b.Recover(context.Background(), "a", v.OrderID, "10001")
	if err != nil || recovered.Status != "waiting" {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
}
func TestCloseProtectionAndDeadline(t *testing.T) {
	b, p, now := fixture(t)
	v := reserve(t, b, "a", "close_guard", "guard")
	_, _ = b.Close("a", v.OrderID)
	b.Tick(context.Background())
	v, _ = b.Status("a", v.OrderID)
	if v.Status != "close_pending" || v.LastError != "NO_COMPLETE_TZID" {
		t.Fatal(v)
	}
	*now = now.Add(121 * time.Second)
	b.Tick(context.Background())
	v, _ = b.Status("a", v.OrderID)
	if v.Status != "closed" {
		t.Fatal(v)
	}
	v, err := b.Reserve(context.Background(), "a", ReserveInput{Service: "late_sms", IdempotencyKey: "late", MaxPrice: "0.20", WaitSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	b.Tick(context.Background())
	v, _ = b.Status("a", v.OrderID)
	if v.Status != "closed" || !p.orders[b.store.state.Orders[v.OrderID].ProviderID].closed {
		t.Fatal("deadline didn't clean up")
	}
}
func TestRestartPreservesIdempotencyAndSecrets(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, "mock")
	if err != nil {
		t.Fatal(err)
	}
	p := NewMockProvider()
	p.Delay = 0
	b, _ := New(p, s, DefaultConfig())
	v := reserve(t, b, "a", "demo", "key")
	b.Tick(context.Background())
	data, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "123456") {
		t.Fatal("OTP persisted")
	}
	info, _ := os.Stat(filepath.Join(dir, "state.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %o", info.Mode().Perm())
	}
	if _, err := OpenStore(dir, "mock"); err == nil {
		t.Fatal("second writer acquired state directory")
	}
	s.Close()
	s, err = OpenStore(dir, "mock")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p = NewMockProvider()
	b, _ = New(p, s, DefaultConfig())
	r := reserve(t, b, "a", "demo", "key")
	if r.OrderID != v.OrderID || r.Code != "" || r.Status != "waiting" {
		t.Fatal("state not restored")
	}
	if p.seq != 1 {
		t.Fatal("mock state not restored")
	}
	if _, err := OpenStore(dir, "live"); err == nil {
		t.Fatal("mixed mock/live state")
	}
}
func TestInterruptedPurchaseRecoveredAsUnknown(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(dir, "mock")
	s.state.Orders["id"] = &Order{ID: "id", Owner: "a", IdempotencyKey: "key", Service: "demo", Country: 91, Status: "purchasing", Estimate: 100000, Created: time.Now(), Deadline: time.Now().Add(time.Minute)}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := OpenStore(dir, "mock")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := NewMockProvider()
	b, _ := New(p, s, DefaultConfig())
	v := reserve(t, b, "a", "demo", "key")
	if v.Status != "purchase_unknown" || p.seq != 0 {
		t.Fatal("reissued interrupted purchase")
	}
}
func TestStateWriteFailureStopsMutations(t *testing.T) {
	b, p, _ := fixture(t)
	b.store.path = filepath.Join(t.TempDir(), "missing", "state.json")
	_, err := b.Reserve(context.Background(), "a", ReserveInput{Service: "demo", IdempotencyKey: "key", MaxPrice: "0.20"})
	if errorCode(err) != "STATE_WRITE_FAILED" || p.seq != 0 {
		t.Fatal("purchased without durable intent")
	}
	if b.Healthy() {
		t.Fatal("storage failure hidden")
	}
	_, err = b.Reserve(context.Background(), "a", ReserveInput{Service: "demo", IdempotencyKey: "other", MaxPrice: "0.20"})
	if errorCode(err) != "STATE_WRITE_FAILED" {
		t.Fatal(err)
	}
}
func TestAmountValidation(t *testing.T) {
	for _, s := range []string{"-1", "NaN", "1e2", "1.0000001", "9999999999", ""} {
		if _, e := ParseAmount(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	a, e := ParseAmount("0.123456")
	if e != nil || a.String() != "0.123456" {
		t.Fatal(a, e)
	}
	if retryable(errors.New("secret-url")) || errorCode(errors.New("secret-url")) != "INTERNAL_ERROR" {
		t.Fatal("unknown error leaked")
	}
}

type pointerTimeout struct {
	Provider
	calls int
}

func (p *pointerTimeout) Next(ctx context.Context, id string) error {
	p.calls++
	_ = p.Provider.Next(ctx, id)
	return &Error{Code: "UPSTREAM_UNAVAILABLE", Uncertain: true}
}
func TestAmbiguousNextMessageIsNotReplayed(t *testing.T) {
	b, p, now := fixture(t)
	v := reserve(t, b, "a", "demo", "key")
	*now = now.Add(3 * time.Second)
	b.Tick(context.Background())
	timeout := &pointerTimeout{Provider: p}
	b.provider = timeout
	_, e := b.Next(context.Background(), "a", v.OrderID)
	if !uncertain(e) {
		t.Fatal(e)
	}
	v, _ = b.Status("a", v.OrderID)
	if v.Code != "" {
		t.Fatal("old OTP exposed after pointer ambiguity")
	}
	_, e = b.Next(context.Background(), "a", v.OrderID)
	if errorCode(e) != "NEXT_MESSAGE_NOT_ALLOWED" || timeout.calls != 1 {
		t.Fatal("pointer replayed")
	}
}
func TestBackgroundLoopReceivesSMSAndCleansUp(t *testing.T) {
	b, p, _ := fixture(t)
	b.now = time.Now
	p.now = time.Now
	p.Delay = 5 * time.Millisecond
	b.config.PollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	v := reserve(t, b, "a", "demo", "key")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, _ = b.Status("a", v.OrderID)
		if v.Status == "code_received" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v.Status != "code_received" {
		t.Fatal("background SMS poll did not run")
	}
	_, e := b.Close("a", v.OrderID)
	if e != nil {
		t.Fatal(e)
	}
	for time.Now().Before(deadline) {
		v, _ = b.Status("a", v.OrderID)
		if v.Status == "closed" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background cleanup did not run")
}
func TestProviderAccountAndPriceUnitCannotChange(t *testing.T) {
	s, e := OpenStore(t.TempDir(), "live")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := NewHTTPProvider("https://onlinesim.io", "fixture-account-a", nil)
	if e != nil {
		t.Fatal(e)
	}
	cfg := DefaultConfig()
	if _, e := New(p, s, cfg); e != nil {
		t.Fatal(e)
	}
	other, _ := NewHTTPProvider("https://onlinesim.io", "fixture-account-b", nil)
	if _, e := New(other, s, cfg); e == nil {
		t.Fatal("account identity changed")
	}
	cfg.Currency = "other_unit"
	if _, e := New(p, s, cfg); e == nil {
		t.Fatal("price unit changed")
	}
}
func TestDiscoveryCannotPurchase(t *testing.T) {
	b, p, _ := fixture(t)
	_, e := b.Reserve(context.Background(), "discovery", ReserveInput{Service: "demo", IdempotencyKey: "key", MaxPrice: "0.20"})
	if errorCode(e) != "TASK_ID_REQUIRED" || p.seq != 0 {
		t.Fatal("discovery context purchased a number")
	}
}
