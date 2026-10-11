package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	MaxOrders    int
	TaskBudget   Amount // Gross tariff estimates; refunds are never assumed.
	MaxPrice     Amount
	Lease        time.Duration
	PollInterval time.Duration
	Currency     string // Explicit profile tariff unit label; no inferred currency.
}

func DefaultConfig() Config {
	return Config{MaxOrders: 2, TaskBudget: 5000000, MaxPrice: 1000000, Lease: 10 * time.Minute, PollInterval: 5 * time.Second, Currency: "profile_tariff_units"}
}

type Broker struct {
	mu            sync.Mutex
	provider      Provider
	store         *Store
	config        Config
	now           func() time.Time
	storageFailed bool
}
type ReserveInput struct {
	Service        string `json:"service" jsonschema:"OnlineSim service slug returned by sms.quote"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Stable key for this registration attempt; reuse it after a timeout"`
	MaxPrice       string `json:"max_price" jsonschema:"Maximum preflight tariff estimate in configured profile tariff units"`
	WaitSeconds    int    `json:"wait_seconds,omitempty" jsonschema:"Optional wait deadline, 1 to 900 seconds; default 600"`
}
type View struct {
	OrderID        string    `json:"order_id"`
	Mode           string    `json:"mode"`
	Country        int       `json:"country"`
	Service        string    `json:"service"`
	Number         string    `json:"number,omitempty"`
	Status         string    `json:"status"`
	Code           string    `json:"code,omitempty"`
	EstimatedPrice string    `json:"estimated_price"`
	PriceUnit      string    `json:"price_unit"`
	Deadline       time.Time `json:"deadline"`
	LastError      string    `json:"last_error,omitempty"`
	CloseRequested bool      `json:"close_requested"`
}

func New(provider Provider, store *Store, config Config) (*Broker, error) {
	if config.MaxOrders < 1 || config.TaskBudget <= 0 || config.MaxPrice <= 0 || config.Lease <= 0 || config.Lease > 15*time.Minute || config.PollInterval <= 0 || config.PollInterval > time.Minute || config.Currency == "" {
		return nil, errors.New("invalid broker limits")
	}
	if p, ok := provider.(*HTTPProvider); ok {
		identity := p.identity()
		if store.state.ProviderIdentity != "" && store.state.ProviderIdentity != identity {
			return nil, errors.New("provider account changed: use a separate state directory")
		}
		if store.state.ProviderIdentity == "" && len(store.state.Orders) > 0 {
			return nil, errors.New("existing live orders have no provider identity")
		}
		store.state.ProviderIdentity = identity
	}
	if store.state.PriceUnit != "" && store.state.PriceUnit != config.Currency {
		return nil, errors.New("price unit changed: use a separate state directory")
	}
	store.state.PriceUnit = config.Currency
	if err := store.Save(); err != nil {
		return nil, err
	}
	if m, ok := provider.(*MockProvider); ok {
		m.restore(store.state.Orders)
	}
	return &Broker{provider: provider, store: store, config: config, now: time.Now}, nil
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func ValidateOwner(owner string) bool { return identifier.MatchString(owner) }
func (b *Broker) save() error {
	if err := b.store.Save(); err != nil {
		b.storageFailed = true
		return &Error{Code: "STATE_WRITE_FAILED"}
	}
	return nil
}
func (b *Broker) view(o *Order) View {
	code := o.Code
	if o.CloseRequested || o.Status == "closed" || o.Status == "expired" {
		code = ""
	}
	return View{OrderID: o.ID, Mode: b.store.state.Mode, Country: o.Country, Service: o.Service, Number: o.Number, Status: o.Status, Code: code, EstimatedPrice: o.Estimate.String(), PriceUnit: b.config.Currency, Deadline: o.Deadline, LastError: o.LastError, CloseRequested: o.CloseRequested}
}
func (b *Broker) Quote(ctx context.Context, service string, page int) (Quote, error) {
	if service != "" && !identifier.MatchString(service) {
		return Quote{}, &Error{Code: "INVALID_SERVICE"}
	}
	if page < 1 || page > 10000 {
		return Quote{}, &Error{Code: "INVALID_PAGE"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q, err := b.provider.Quote(ctx, 91, service, page)
	sort.Slice(q.Tariffs, func(i, j int) bool { return q.Tariffs[i].Service < q.Tariffs[j].Service })
	return q, err
}
func (b *Broker) Reserve(ctx context.Context, owner string, in ReserveInput) (View, error) {
	if owner == "discovery" {
		return View{}, &Error{Code: "TASK_ID_REQUIRED"}
	}
	if !ValidateOwner(owner) || !identifier.MatchString(in.Service) || !identifier.MatchString(in.IdempotencyKey) {
		return View{}, &Error{Code: "INVALID_RESERVATION"}
	}
	max, err := ParseAmount(in.MaxPrice)
	if err != nil || max <= 0 {
		return View{}, &Error{Code: "INVALID_MAX_PRICE"}
	}
	lease := b.config.Lease
	if in.WaitSeconds != 0 {
		if in.WaitSeconds < 1 || in.WaitSeconds > 900 {
			return View{}, &Error{Code: "INVALID_WAIT_SECONDS"}
		}
		lease = time.Duration(in.WaitSeconds) * time.Second
		if lease > b.config.Lease {
			lease = b.config.Lease
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.storageFailed {
		return View{}, &Error{Code: "STATE_WRITE_FAILED"}
	}
	count := 0
	var reserved Amount
	for _, o := range b.store.state.Orders {
		if o.Owner != owner {
			continue
		}
		if o.IdempotencyKey == in.IdempotencyKey {
			if o.Service != in.Service {
				return View{}, &Error{Code: "IDEMPOTENCY_CONFLICT"}
			}
			return b.view(o), nil
		}
		count++
		if o.Status != "failed" {
			reserved += o.Estimate
		}
		if o.Status == "purchase_unknown" || o.Status == "purchasing" {
			return View{}, &Error{Code: "PURCHASE_RECONCILIATION_REQUIRED"}
		}
	}
	if count >= b.config.MaxOrders {
		return View{}, &Error{Code: "ORDER_LIMIT_EXCEEDED"}
	}
	q, err := b.provider.Quote(ctx, 91, in.Service, 1)
	if err != nil {
		return View{}, err
	}
	var tariff *Tariff
	for i := range q.Tariffs {
		if q.Tariffs[i].Service == in.Service && q.Tariffs[i].Country == 91 {
			tariff = &q.Tariffs[i]
			break
		}
	}
	if tariff == nil {
		return View{}, &Error{Code: "SERVICE_NOT_AVAILABLE"}
	}
	if tariff.Available < 1 {
		return View{}, &Error{Code: "NO_NUMBER"}
	}
	price, err := ParseAmount(tariff.Price)
	if err != nil {
		return View{}, &Error{Code: "INVALID_TARIFF_PRICE"}
	}
	if price > max || price > b.config.MaxPrice {
		return View{}, &Error{Code: "PRICE_LIMIT_EXCEEDED"}
	}
	if reserved+price > b.config.TaskBudget {
		return View{}, &Error{Code: "BUDGET_LIMIT_EXCEEDED"}
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return View{}, err
	}
	now := b.now()
	o := &Order{ID: hex.EncodeToString(bytes), Owner: owner, IdempotencyKey: in.IdempotencyKey, Country: 91, Service: in.Service, ServiceName: tariff.Name, Status: "purchasing", Estimate: price, Created: now, Deadline: now.Add(lease)}
	b.store.state.Orders[o.ID] = o
	// Durable intent before calling the non-idempotent upstream getNum.
	if err := b.save(); err != nil {
		return View{}, err
	}
	activation, err := b.provider.Buy(ctx, 91, in.Service)
	if err != nil {
		o.Status = "failed"
		o.LastError = errorCode(err)
		if uncertain(err) {
			o.Status = "purchase_unknown"
			o.LastError = "PURCHASE_OUTCOME_UNKNOWN"
		}
		if saveErr := b.save(); saveErr != nil {
			return View{}, saveErr
		}
		return b.view(o), nil
	}
	if activation.ID == "" {
		o.Status = "purchase_unknown"
		o.LastError = "PURCHASE_OUTCOME_UNKNOWN"
	} else {
		o.ProviderID = activation.ID
		o.Number = activation.Number
		o.Status = "waiting"
	}
	if err := b.save(); err != nil {
		return View{}, err
	}
	return b.view(o), nil
}
func (b *Broker) owned(owner, id string) (*Order, error) {
	o, ok := b.store.state.Orders[id]
	if !ok || o.Owner != owner {
		return nil, &Error{Code: "ORDER_NOT_FOUND"}
	}
	return o, nil
}
func (b *Broker) Status(owner, id string) (View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, err := b.owned(owner, id)
	if err != nil {
		return View{}, err
	}
	return b.view(o), nil
}
func (b *Broker) Close(owner, id string) (View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.storageFailed {
		return View{}, &Error{Code: "STATE_WRITE_FAILED"}
	}
	o, err := b.owned(owner, id)
	if err != nil {
		return View{}, err
	}
	if o.Status == "closed" || o.Status == "expired" || o.Status == "failed" {
		return b.view(o), nil
	}
	if o.Status == "purchase_unknown" || o.ProviderID == "" {
		return View{}, &Error{Code: "PURCHASE_RECONCILIATION_REQUIRED"}
	}
	o.CloseRequested = true
	o.Status = "close_pending"
	o.NextAttempt = time.Time{}
	if err := b.save(); err != nil {
		return View{}, err
	}
	return b.view(o), nil
}
func (b *Broker) Next(ctx context.Context, owner, id string) (View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.storageFailed {
		return View{}, &Error{Code: "STATE_WRITE_FAILED"}
	}
	o, err := b.owned(owner, id)
	if err != nil {
		return View{}, err
	}
	if o.CloseRequested || o.Status != "code_received" || o.NextPending || !b.now().Before(o.Deadline) {
		return View{}, &Error{Code: "NEXT_MESSAGE_NOT_ALLOWED"}
	}
	// Pointer advancement is not idempotent. Persist before the call and do not retry it.
	o.NextPending = true
	if err := b.save(); err != nil {
		return View{}, err
	}
	err = b.provider.Next(ctx, o.ProviderID)
	if err != nil {
		o.LastError = errorCode(err)
		if uncertain(err) {
			o.Code = ""
			o.Status = "waiting"
			o.LastError = "NEXT_MESSAGE_OUTCOME_UNKNOWN"
		}
		if !uncertain(err) {
			o.NextPending = false
		}
		if e := b.save(); e != nil {
			return View{}, e
		}
		return View{}, err
	}
	o.NextPending = false
	o.Code = ""
	o.Status = "waiting"
	o.LastError = ""
	o.NextAttempt = time.Time{}
	if err := b.save(); err != nil {
		return View{}, err
	}
	return b.view(o), nil
}

// Recover is operator-only: never exposed as an Agent tool. The operator must
// verify the ambiguous purchase against the OnlineSim dashboard first.
func (b *Broker) Recover(ctx context.Context, owner, id, providerID string) (View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.storageFailed {
		return View{}, &Error{Code: "STATE_WRITE_FAILED"}
	}
	o, err := b.owned(owner, id)
	if err != nil {
		return View{}, err
	}
	if o.Status != "purchase_unknown" || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(providerID) {
		return View{}, &Error{Code: "INVALID_RECOVERY"}
	}
	for _, other := range b.store.state.Orders {
		if other.ProviderID == providerID {
			return View{}, &Error{Code: "PROVIDER_ORDER_ALREADY_BOUND"}
		}
	}
	s, err := b.provider.State(ctx, providerID)
	if err != nil {
		return View{}, err
	}
	if s.Country != o.Country || !matchesService(s.Service, o) {
		return View{}, &Error{Code: "RECOVERY_SCOPE_MISMATCH"}
	}
	o.ProviderID = providerID
	o.Number = s.Number
	o.Status = "waiting"
	o.LastError = ""
	if err := b.save(); err != nil {
		return View{}, err
	}
	return b.view(o), nil
}
func (b *Broker) backoff(o *Order, now time.Time) {
	o.Failures++
	shift := o.Failures
	if shift > 6 {
		shift = 6
	}
	delay := b.config.PollInterval * time.Duration(1<<shift)
	if delay > time.Minute {
		delay = time.Minute
	}
	o.NextAttempt = now.Add(delay)
}
func (b *Broker) Tick(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.storageFailed {
		return
	}
	changed := false
	for _, o := range b.store.state.Orders {
		if ctx.Err() != nil {
			break
		}
		if o.ProviderID == "" || o.Status == "closed" || o.Status == "expired" || o.Status == "failed" {
			continue
		}
		now := b.now()
		if !now.Before(o.Deadline) && !o.CloseRequested {
			o.CloseRequested = true
			o.Status = "close_pending"
			o.NextAttempt = time.Time{}
			changed = true
		}
		if now.Before(o.NextAttempt) {
			continue
		}
		if o.CloseRequested {
			err := b.provider.Close(ctx, o.ProviderID)
			switch {
			case err == nil || errorCode(err) == "ERROR_NO_TZID" || errorCode(err) == "ERROR_NO_OPERATIONS":
				o.Status = "closed"
				o.Code = ""
				o.LastError = ""
			default:
				o.Status = "close_pending"
				o.LastError = errorCode(err)
				b.backoff(o, now)
			}
			changed = true
			continue
		}
		s, err := b.provider.State(ctx, o.ProviderID)
		if err != nil {
			o.LastError = errorCode(err)
			if o.LastError == "ERROR_NO_TZID" || o.LastError == "ERROR_NO_OPERATIONS" {
				o.Status = "expired"
				o.Code = ""
			} else {
				b.backoff(o, now)
			}
			changed = true
			continue
		}
		// Reject cross-country state even if the provider returns a mismatched tzid.
		if (s.Country != 0 && s.Country != o.Country) || (s.Service != "" && !matchesService(s.Service, o)) {
			o.LastError = "STATE_SCOPE_MISMATCH"
			b.backoff(o, now)
			changed = true
			continue
		}
		o.LastError = ""
		o.Failures = 0
		o.NextAttempt = now.Add(b.config.PollInterval)
		if s.Number != "" {
			o.Number = s.Number
		}
		if s.Remaining > 0 && now.Add(time.Duration(s.Remaining)*time.Second).Before(o.Deadline) {
			o.Deadline = now.Add(time.Duration(s.Remaining) * time.Second)
		}
		switch s.Status {
		case "TZ_OVER_OK":
			o.Status = "closed"
			o.Code = ""
		case "TZ_OVER_EMPTY":
			o.Status = "expired"
			o.Code = ""
		default:
			if s.Code != "" {
				o.Status = "code_received"
				o.Code = s.Code
			} else {
				o.Status = "waiting"
				o.Code = ""
			}
			if s.Remaining <= 0 {
				o.CloseRequested = true
				o.Status = "close_pending"
			}
		}
		changed = true
	}
	if changed {
		_ = b.save()
	}
}
func (b *Broker) Run(ctx context.Context) {
	ticker := time.NewTicker(b.config.PollInterval)
	defer ticker.Stop()
	for {
		b.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (b *Broker) Healthy() bool { b.mu.Lock(); defer b.mu.Unlock(); return !b.storageFailed }

func matchesService(service string, o *Order) bool {
	return strings.EqualFold(service, o.Service) || (o.ServiceName != "" && strings.EqualFold(service, o.ServiceName))
}
