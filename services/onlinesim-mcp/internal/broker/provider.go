package broker

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Amount uses millionths of the provider profile's tariff units, never floats.
type Amount int64

func ParseAmount(s string) (Amount, error) {
	if !regexp.MustCompile(`^[0-9]{1,9}(\.[0-9]{1,6})?$`).MatchString(s) {
		return 0, errors.New("amount must be a non-negative decimal with at most 6 decimal places")
	}
	p := strings.SplitN(s, ".", 2)
	whole, _ := strconv.ParseInt(p[0], 10, 64)
	var frac int64
	if len(p) == 2 {
		frac, _ = strconv.ParseInt(p[1]+strings.Repeat("0", 6-len(p[1])), 10, 64)
	}
	return Amount(whole*1000000 + frac), nil
}
func (a Amount) String() string { return fmt.Sprintf("%d.%06d", a/1000000, a%1000000) }

type Error struct {
	Code      string
	Retryable bool
	Uncertain bool
}

func (e *Error) Error() string { return e.Code }
func errorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "INTERNAL_ERROR"
}
func retryable(err error) bool { var e *Error; return errors.As(err, &e) && e.Retryable }
func uncertain(err error) bool { var e *Error; return !errors.As(err, &e) || e.Uncertain }
func providerError(code string) error {
	if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,79}$`).MatchString(code) {
		return &Error{Code: "INVALID_UPSTREAM_RESPONSE", Uncertain: true}
	}
	if !knownError(code) && code != "TRY_AGAIN_LATER" {
		return &Error{Code: "UNKNOWN_UPSTREAM_ERROR", Uncertain: true}
	}
	retry := code == "NO_COMPLETE_TZID" || code == "INTERVAL_CONCURRENT_REQUESTS_ERROR" || code == "TRY_AGAIN_LATER"
	// TRY_AGAIN_LATER does not prove that a purchase wasn't performed.
	return &Error{Code: code, Retryable: retry, Uncertain: code == "TRY_AGAIN_LATER" || !knownError(code)}
}
func knownError(code string) bool {
	switch code {
	case "NO_NUMBER", "WARNING_LOW_BALANCE", "ACCOUNT_BLOCKED", "API_ACCESS_DISABLED", "API_ACCESS_IP", "ERROR_WRONG_KEY", "ERROR_NO_KEY", "UNDEFINED_COUNTRY", "ERROR_NO_SERVICE", "NO_COMPLETE_TZID", "INTERVAL_CONCURRENT_REQUESTS_ERROR", "ERROR_NO_TZID", "ERROR_NO_OPERATIONS", "REQUEST_NOT_FOUND":
		return true
	}
	return false
}

type Tariff struct {
	Service   string `json:"service"`
	Name      string `json:"name"`
	Country   int    `json:"country"`
	Available int    `json:"available"`
	Price     string `json:"price"`
}
type Quote struct {
	Tariffs []Tariff `json:"tariffs"`
	Page    int      `json:"page"`
	HasMore bool     `json:"has_more"`
}
type Activation struct {
	ID     string
	Number string
}
type State struct {
	ID        string
	Number    string
	Status    string
	Code      string
	Remaining int
	Country   int
	Service   string
}
type Provider interface {
	Quote(context.Context, int, string, int) (Quote, error)
	Buy(context.Context, int, string) (Activation, error)
	State(context.Context, string) (State, error)
	Next(context.Context, string) error
	Close(context.Context, string) error
}

type HTTPProvider struct {
	base   *url.URL
	key    string
	client *http.Client
}

func NewHTTPProvider(baseURL, key string, client *http.Client) (*HTTPProvider, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return nil, errors.New("provider URL must use HTTPS (HTTP loopback allowed for tests)")
	}
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("provider API key is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	copyClient := *client
	// OnlineSim uses GET even for purchases and pointer changes. Go may retry a
	// GET on a broken reused connection. Fresh HTTP/1 connections avoid that
	// replay path and HTTP/2 GOAWAY retries for these non-idempotent operations.
	var transport *http.Transport
	if client.Transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	} else if t, ok := client.Transport.(*http.Transport); ok {
		transport = t.Clone()
	} else {
		return nil, errors.New("provider client requires an HTTP transport")
	}
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	copyClient.Transport = transport
	// No credential forwarding, implicit purchase retry, or following redirects.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPProvider{base: u, key: key, client: &copyClient}, nil
}
func (p *HTTPProvider) call(ctx context.Context, method string, q url.Values) (json.RawMessage, error) {
	u := *p.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/" + method + ".php"
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &Error{Code: "REQUEST_FAILED", Uncertain: true}
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &Error{Code: "UPSTREAM_UNAVAILABLE", Retryable: true, Uncertain: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &Error{Code: fmt.Sprintf("UPSTREAM_HTTP_%d", resp.StatusCode), Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500, Uncertain: true}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 || !json.Valid(data) {
		return nil, &Error{Code: "INVALID_UPSTREAM_RESPONSE", Uncertain: true}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) == nil {
		if raw, ok := obj["response"]; ok {
			code := scalar(raw)
			if code != "1" {
				if !regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,79}$`).MatchString(code) {
					return nil, &Error{Code: "INVALID_UPSTREAM_RESPONSE", Uncertain: true}
				}
				return nil, providerError(code)
			}
		}
	}
	return data, nil
}
func scalar(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
func integer(raw json.RawMessage) int { n, _ := strconv.Atoi(scalar(raw)); return n }
func (p *HTTPProvider) Quote(ctx context.Context, country int, service string, page int) (Quote, error) {
	q := url.Values{"country": {strconv.Itoa(country)}, "filter_country": {strconv.Itoa(country)}, "page": {strconv.Itoa(page)}, "lang": {"en"}}
	if service != "" {
		q.Set("filter_service", service)
	}
	data, err := p.call(ctx, "getTariffs", q)
	if err != nil {
		return Quote{}, err
	}
	var obj struct {
		Response json.RawMessage `json:"response"`
		Country  json.RawMessage `json:"country"`
		Services map[string]struct {
			Service string          `json:"service"`
			Slug    string          `json:"slug"`
			Count   json.RawMessage `json:"count"`
			Price   json.RawMessage `json:"price"`
		} `json:"services"`
		End *bool `json:"end"`
	}
	if json.Unmarshal(data, &obj) != nil || scalar(obj.Response) != "1" || obj.Services == nil || integer(obj.Country) != country || obj.End == nil {
		return Quote{}, &Error{Code: "INVALID_TARIFF_RESPONSE"}
	}
	result := Quote{Tariffs: []Tariff{}, Page: page, HasMore: !*obj.End}
	for _, t := range obj.Services {
		if t.Slug == "" {
			continue
		}
		price := scalar(t.Price)
		if _, err := ParseAmount(price); err != nil {
			return Quote{}, &Error{Code: "INVALID_TARIFF_PRICE"}
		}
		result.Tariffs = append(result.Tariffs, Tariff{Service: t.Slug, Name: t.Service, Country: country, Available: integer(t.Count), Price: price})
	}
	return result, nil
}
func (p *HTTPProvider) Buy(ctx context.Context, country int, service string) (Activation, error) {
	data, err := p.call(ctx, "getNum", url.Values{"country": {strconv.Itoa(country)}, "service": {service}, "number": {"true"}, "lang": {"en"}})
	if err != nil {
		return Activation{}, err
	}
	var obj struct {
		Response json.RawMessage `json:"response"`
		ID       json.RawMessage `json:"tzid"`
		Number   json.RawMessage `json:"number"`
	}
	if json.Unmarshal(data, &obj) != nil || scalar(obj.Response) != "1" || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(scalar(obj.ID)) {
		return Activation{}, &Error{Code: "INVALID_PURCHASE_RESPONSE", Uncertain: true}
	}
	return Activation{ID: scalar(obj.ID), Number: scalar(obj.Number)}, nil
}
func (p *HTTPProvider) State(ctx context.Context, id string) (State, error) {
	data, err := p.call(ctx, "getState", url.Values{"tzid": {id}, "message_to_code": {"1"}, "msg_list": {"0"}, "clean": {"0"}, "orderby": {"desc"}, "lang": {"en"}})
	if err != nil {
		return State{}, err
	}
	var list []map[string]json.RawMessage
	if json.Unmarshal(data, &list) != nil {
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(data, &obj)
		// Some deployments wrap the operations in data; unknown wrappers fail closed.
		if scalar(obj["response"]) != "1" || json.Unmarshal(obj["data"], &list) != nil {
			return State{}, &Error{Code: "INVALID_STATE_RESPONSE", Uncertain: true}
		}
	}
	for _, o := range list {
		if scalar(o["tzid"]) != id {
			continue
		}
		status := scalar(o["response"])
		switch status {
		case "TZ_INPOOL", "TZ_POOL", "TZ_NUM_WAIT", "TZ_NUM_ANSWER", "TZ_OVER_EMPTY", "TZ_OVER_OK":
		default:
			return State{}, providerError(status)
		}
		if _, ok := o["time"]; !ok && status != "TZ_OVER_EMPTY" && status != "TZ_OVER_OK" {
			return State{}, &Error{Code: "INVALID_STATE_RESPONSE", Uncertain: true}
		}
		return State{ID: id, Number: scalar(o["number"]), Status: status, Code: scalar(o["msg"]), Remaining: integer(o["time"]), Country: integer(o["country"]), Service: scalar(o["service"])}, nil
	}
	return State{}, &Error{Code: "ERROR_NO_TZID"}
}
func (p *HTTPProvider) action(ctx context.Context, method, id string) error {
	data, err := p.call(ctx, method, url.Values{"tzid": {id}, "lang": {"en"}})
	if err != nil {
		return err
	}
	var o map[string]json.RawMessage
	_ = json.Unmarshal(data, &o)
	if scalar(o["response"]) != "1" {
		return &Error{Code: "INVALID_ACTION_RESPONSE", Uncertain: true}
	}
	return nil
}
func (p *HTTPProvider) Next(ctx context.Context, id string) error {
	return p.action(ctx, "setOperationRevise", id)
}
func (p *HTTPProvider) Close(ctx context.Context, id string) error {
	return p.action(ctx, "setOperationOk", id)
}

func (p *HTTPProvider) identity() string {
	sum := sha256.Sum256([]byte(p.base.String() + "\x00" + p.key))
	return hex.EncodeToString(sum[:])
}
