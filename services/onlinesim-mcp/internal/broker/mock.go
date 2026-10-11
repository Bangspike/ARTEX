package broker

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type mockOrder struct {
	created  time.Time
	revision int
	closed   bool
	service  string
}

// MockProvider never performs network requests. Its messages are synthetic fixtures.
type MockProvider struct {
	mu     sync.Mutex
	orders map[string]*mockOrder
	seq    int
	now    func() time.Time
	Delay  time.Duration
	Guard  time.Duration
}

func NewMockProvider() *MockProvider {
	return &MockProvider{orders: map[string]*mockOrder{}, now: time.Now, Delay: 2 * time.Second, Guard: 120 * time.Second}
}
func (p *MockProvider) Quote(ctx context.Context, country int, service string, page int) (Quote, error) {
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}
	q := Quote{Tariffs: []Tariff{}, Page: page}
	if country != 91 {
		return q, providerError("UNDEFINED_COUNTRY")
	}
	for _, slug := range []string{"demo", "no_stock", "late_sms", "close_guard", "purchase_timeout"} {
		if service != "" && service != slug {
			continue
		}
		count := 100
		if slug == "no_stock" {
			count = 0
		}
		q.Tariffs = append(q.Tariffs, Tariff{Service: slug, Name: slug, Country: 91, Available: count, Price: "0.10"})
	}
	return q, nil
}
func (p *MockProvider) Buy(ctx context.Context, country int, service string) (Activation, error) {
	if service == "no_stock" {
		return Activation{}, providerError("NO_NUMBER")
	}
	if err := ctx.Err(); err != nil {
		return Activation{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	id := fmt.Sprintf("%d", 10000+p.seq)
	p.orders[id] = &mockOrder{created: p.now(), service: service}
	if service == "purchase_timeout" {
		return Activation{}, &Error{Code: "UPSTREAM_UNAVAILABLE", Retryable: true, Uncertain: true}
	}
	return Activation{ID: id, Number: "+910000000000"}, nil
}
func (p *MockProvider) State(ctx context.Context, id string) (State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok {
		return State{}, providerError("ERROR_NO_TZID")
	}
	if o.closed {
		return State{ID: id, Status: "TZ_OVER_OK", Country: 91}, nil
	}
	s := State{ID: id, Number: "+910000000000", Country: 91, Service: o.service, Status: "TZ_NUM_WAIT", Remaining: 900 - int(p.now().Sub(o.created).Seconds())}
	if o.service != "late_sms" && o.service != "close_guard" && p.now().Sub(o.created) >= p.Delay {
		s.Status = "TZ_NUM_ANSWER"
		s.Code = fmt.Sprintf("%06d", 123456+o.revision)
	}
	return s, nil
}
func (p *MockProvider) Next(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok {
		return providerError("ERROR_NO_TZID")
	}
	o.revision++
	return nil
}
func (p *MockProvider) Close(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[id]
	if !ok {
		return providerError("ERROR_NO_TZID")
	}
	if o.service == "close_guard" && p.now().Sub(o.created) < p.Guard {
		return providerError("NO_COMPLETE_TZID")
	}
	o.closed = true
	return nil
}

func (p *MockProvider) restore(orders map[string]*Order) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range orders {
		if o.ProviderID == "" {
			continue
		}
		var n int
		_, _ = fmt.Sscanf(o.ProviderID, "%d", &n)
		if n-10000 > p.seq {
			p.seq = n - 10000
		}
		if _, ok := p.orders[o.ProviderID]; !ok {
			p.orders[o.ProviderID] = &mockOrder{created: o.Created, service: o.Service, closed: o.Status == "closed" || o.Status == "expired"}
		}
	}
}
