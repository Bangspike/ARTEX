package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Order struct {
	ID             string    `json:"order_id"`
	Owner          string    `json:"owner"`
	IdempotencyKey string    `json:"idempotency_key"`
	Country        int       `json:"country"`
	Service        string    `json:"service"`
	ServiceName    string    `json:"service_name,omitempty"`
	ProviderID     string    `json:"provider_id,omitempty"`
	Number         string    `json:"number,omitempty"`
	Status         string    `json:"status"`
	Estimate       Amount    `json:"estimate"`
	Created        time.Time `json:"created_at"`
	Deadline       time.Time `json:"deadline"`
	CloseRequested bool      `json:"close_requested"`
	LastError      string    `json:"last_error,omitempty"`
	NextPending    bool      `json:"next_pending,omitempty"`
	Code           string    `json:"-"` // OTPs are ephemeral and never written to disk.
	NextAttempt    time.Time `json:"-"`
	Failures       int       `json:"-"`
}
type diskState struct {
	ProviderIdentity string            `json:"provider_identity,omitempty"`
	PriceUnit        string            `json:"price_unit,omitempty"`
	Version          int               `json:"version"`
	Mode             string            `json:"mode"`
	Orders           map[string]*Order `json:"orders"`
}
type Store struct {
	path  string
	lock  *os.File
	state diskState
}

func OpenStore(dir, mode string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "broker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockStateFile(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cannot lock state directory: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "state.json"), lock: lock, state: diskState{Version: 1, Mode: mode, Orders: map[string]*Order{}}}
	data, err := os.ReadFile(s.path)
	if err == nil {
		if json.Unmarshal(data, &s.state) != nil || s.state.Version != 1 || s.state.Mode != mode || s.state.Orders == nil {
			s.Close()
			return nil, errors.New("invalid state or mode mismatch: use a separate data directory for mock/live")
		}
	}
	if err != nil && !os.IsNotExist(err) {
		s.Close()
		return nil, err
	}
	// A crash while purchasing is ambiguous; never silently retry the purchase.
	for id, o := range s.state.Orders {
		if o == nil || o.ID != id || o.Owner == "" {
			s.Close()
			return nil, errors.New("invalid order state")
		}
		if o.Status == "purchasing" {
			o.Status = "purchase_unknown"
			o.LastError = "PURCHASE_OUTCOME_UNKNOWN"
		}
		if o.Status == "code_received" {
			o.Status = "waiting"
		}
	}
	if err := s.Save(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Save() error {
	data, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
