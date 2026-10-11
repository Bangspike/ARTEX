package broker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPAdapterLifecycle(t *testing.T) {
	var buys atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" || r.URL.Query().Has("apikey") {
			t.Error("bad credential placement")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/getTariffs.php":
			if r.URL.Query().Get("country") != "91" || r.URL.Query().Get("page") != "2" {
				t.Error("tariff query")
			}
			fmt.Fprint(w, `{"response":"1","country":"91","end":false,"services":{"_demo":{"slug":"demo","service":"Demo","count":"2","price":"0.12"}}}`)
		case "/api/getNum.php":
			buys.Add(1)
			if r.URL.Query().Get("number") != "true" || r.URL.Query().Get("service") != "demo" {
				t.Error("buy query")
			}
			fmt.Fprint(w, `{"response":1,"tzid":"10001","number":"+910000000000"}`)
		case "/api/getState.php":
			if r.URL.Query().Get("tzid") != "10001" || r.URL.Query().Get("message_to_code") != "1" || r.URL.Query().Get("clean") != "0" {
				t.Error("state query")
			}
			fmt.Fprint(w, `[{"tzid":999,"response":"TZ_NUM_ANSWER","msg":"foreign-code","time":100,"country":91},{"tzid":10001,"number":"+910000000000","response":"TZ_NUM_ANSWER","msg":"012345","time":"60","country":"91","service":"demo"}]`)
		case "/api/setOperationRevise.php", "/api/setOperationOk.php":
			fmt.Fprint(w, `{"response":"1","tzid":10001}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p, e := NewHTTPProvider(upstream.URL, "fixture-secret", nil)
	if e != nil {
		t.Fatal(e)
	}
	q, e := p.Quote(context.Background(), 91, "demo", 2)
	if e != nil || !q.HasMore || len(q.Tariffs) != 1 || q.Tariffs[0].Price != "0.12" {
		t.Fatal(q, e)
	}
	a, e := p.Buy(context.Background(), 91, "demo")
	if e != nil || a.ID != "10001" || buys.Load() != 1 {
		t.Fatal(a, e)
	}
	s, e := p.State(context.Background(), a.ID)
	if e != nil || s.Code != "012345" || s.Remaining != 60 || s.Country != 91 {
		t.Fatal(s, e)
	}
	if e := p.Next(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
	if e := p.Close(context.Background(), a.ID); e != nil {
		t.Fatal(e)
	}
}
func TestHTTP200BusinessErrorsAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		body, code string
		ambiguous  bool
	}{{`{"response":"NO_NUMBER"}`, "NO_NUMBER", false}, {`{"response":"ERROR_WRONG_KEY"}`, "ERROR_WRONG_KEY", false}, {`{"response":"TRY_AGAIN_LATER"}`, "TRY_AGAIN_LATER", true}, {`{"response":"NEW_UNKNOWN_ERROR"}`, "UNKNOWN_UPSTREAM_ERROR", true}, {`{"response":1}`, "INVALID_PURCHASE_RESPONSE", true}, {`{"response":"fixture-secret"}`, "INVALID_UPSTREAM_RESPONSE", true}, {`not-json fixture-secret`, "INVALID_UPSTREAM_RESPONSE", true}} {
		t.Run(tc.code, func(t *testing.T) {
			var calls int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, tc.body) }))
			defer s.Close()
			p, _ := NewHTTPProvider(s.URL, "fixture-secret", nil)
			_, e := p.Buy(context.Background(), 91, "demo")
			if errorCode(e) != tc.code || uncertain(e) != tc.ambiguous || calls != 1 {
				t.Fatal(e, calls)
			}
			if strings.Contains(e.Error(), "fixture-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestRedirectIsNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer source.Close()
	p, _ := NewHTTPProvider(source.URL, "fixture-secret", nil)
	_, err := p.Buy(context.Background(), 91, "demo")
	if errorCode(err) != "UPSTREAM_HTTP_302" || leaked.Load() {
		t.Fatal("redirect followed", err)
	}
}
func TestGetNumTimeoutIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h := w.(http.Hijacker)
		conn, _, _ := h.Hijack()
		conn.Close()
	}))
	defer s.Close()
	p, _ := NewHTTPProvider(s.URL, "fixture-secret", nil)
	_, e := p.Buy(context.Background(), 91, "demo")
	if !uncertain(e) || calls.Load() != 1 {
		t.Fatal(e, calls.Load())
	}
}
func TestEmptyAndWrappedState(t *testing.T) {
	for _, tc := range []struct{ body, code string }{{`[]`, "ERROR_NO_TZID"}, {`{"response":"1","data":[{"tzid":"1","response":"TZ_OVER_EMPTY","country":91}]}`, ""}, {`{"response":"1","unexpected":[]}`, "INVALID_STATE_RESPONSE"}, {`[{"tzid":1,"response":"FUTURE_STATE","time":100}]`, "UNKNOWN_UPSTREAM_ERROR"}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
		p, _ := NewHTTPProvider(s.URL, "fixture-secret", nil)
		_, e := p.State(context.Background(), "1")
		s.Close()
		if tc.code == "" && e != nil {
			t.Fatal(e)
		}
		if tc.code != "" && errorCode(e) != tc.code {
			t.Fatalf("%s: %v", tc.body, e)
		}
	}
}

func TestPurchaseUsesFreshConnectionAfterQuote(t *testing.T) {
	var buys atomic.Int32
	var connections atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/getTariffs.php" {
			fmt.Fprint(w, `{"response":1,"country":91,"end":true,"services":{"_demo":{"slug":"demo","service":"Demo","count":1,"price":"0.10"}}}`)
			return
		}
		buys.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	s.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	p, _ := NewHTTPProvider(s.URL, "fixture-secret", nil)
	if _, e := p.Quote(context.Background(), 91, "demo", 1); e != nil {
		t.Fatal(e)
	}
	_, e := p.Buy(context.Background(), 91, "demo")
	if !uncertain(e) || buys.Load() != 1 || connections.Load() != 2 {
		t.Fatalf("implicit replay or reused connection: buys=%d connections=%d err=%v", buys.Load(), connections.Load(), e)
	}
}

func TestUnknownBusinessErrorCannotEchoAPIKey(t *testing.T) {
	const key = "UPSTREAM_SECRET"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"response":%q}`, key) }))
	defer s.Close()
	p, _ := NewHTTPProvider(s.URL, key, nil)
	_, e := p.Buy(context.Background(), 91, "demo")
	if e == nil || strings.Contains(e.Error(), key) || errorCode(e) != "UNKNOWN_UPSTREAM_ERROR" {
		t.Fatal("unknown upstream response echoed API Key")
	}
}
