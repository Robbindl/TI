package selection

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	mu    sync.RWMutex
	path  string
	prefs map[string]Preference
}

type Preference struct {
	Enabled bool   `json:"enabled"`
	Since   string `json:"since,omitempty"`
}

func Open(path string) (*Store, error) {
	store := &Store{path: path, prefs: make(map[string]Preference)}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for jid, value := range raw {
		var enabled bool
		if err := json.Unmarshal(value, &enabled); err == nil {
			store.prefs[jid] = Preference{Enabled: enabled}
			continue
		}
		var preference Preference
		if err := json.Unmarshal(value, &preference); err != nil {
			return nil, err
		}
		store.prefs[jid] = preference
	}
	return store, nil
}

func (s *Store) Enabled(jid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prefs[jid].Enabled
}

func (s *Store) Since(jid string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prefs[jid].Since
}

func (s *Store) Toggle(jid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	preference := s.prefs[jid]
	preference.Enabled = !preference.Enabled
	s.prefs[jid] = preference
	if err := s.saveLocked(); err != nil {
		preference.Enabled = !preference.Enabled
		s.prefs[jid] = preference
		return preference.Enabled, err
	}
	return preference.Enabled, nil
}

func (s *Store) SetEnabled(jid string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	preference := s.prefs[jid]
	preference.Enabled = enabled
	s.prefs[jid] = preference
	if err := s.saveLocked(); err != nil {
		return err
	}
	return nil
}

func (s *Store) SetSince(jid, since string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	preference := s.prefs[jid]
	preference.Since = since
	s.prefs[jid] = preference
	if err := s.saveLocked(); err != nil {
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.prefs, "", "  ")
	if err != nil {
		return err
	}
	tempPath := s.path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}
