package planning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists the full plan book as one JSON file with atomic replacement,
// following the journal snapshot store pattern.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("planning store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create planning directory: %w", err)
	}
	return &Store{path: path}, nil
}

func (s *Store) Load() ([]Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read planning store: %w", err)
	}
	var plans []Plan
	if err := json.Unmarshal(data, &plans); err != nil {
		return nil, fmt.Errorf("decode planning store: %w", err)
	}
	return plans, nil
}

func (s *Store) Save(plans []Plan) error {
	if plans == nil {
		plans = []Plan{}
	}
	data, err := json.MarshalIndent(plans, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	temporary := s.path + ".next"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o640); err != nil {
		return fmt.Errorf("write planning store: %w", err)
	}
	if err := os.Rename(temporary, s.path); err != nil {
		return fmt.Errorf("replace planning store: %w", err)
	}
	return nil
}
