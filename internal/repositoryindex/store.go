// Package repositoryindex stores durable repository graph indexing state.
package repositoryindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// State records the latest graph indexing outcome for one repository.
type State struct {
	Repository    string    `json:"repository"`
	IndexedSHA    string    `json:"indexed_sha,omitempty"`
	ContentDigest string    `json:"content_digest,omitempty"`
	Status        string    `json:"status"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	IndexedAt     time.Time `json:"indexed_at,omitempty"`
}

type persisted struct {
	States []State `json:"states"`
}

// Store is an atomically persisted, repository-keyed index state store.
type Store struct {
	path   string
	mu     sync.RWMutex
	states map[string]State
}

func New(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("repository index data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create repository index data directory: %w", err)
	}
	s := &Store{path: filepath.Join(dataDir, "repository-index.json"), states: make(map[string]State)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns a copy of the recorded state, or nil when the repository is unknown.
func (s *Store) Get(repository string) (*State, error) {
	if strings.TrimSpace(repository) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[repository]
	if !ok {
		return nil, nil
	}
	return &state, nil
}

// Save records a state and atomically replaces the persisted store.
func (s *Store) Save(state State) (*State, error) {
	if strings.TrimSpace(state.Repository) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	if strings.TrimSpace(state.Status) == "" {
		return nil, fmt.Errorf("repository index status is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if previous, ok := s.states[state.Repository]; ok {
		state.CreatedAt = previous.CreatedAt
	} else if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	state.UpdatedAt = now
	if err := s.saveLocked(state); err != nil {
		return nil, err
	}
	s.states[state.Repository] = state
	return &state, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read repository index state: %w", err)
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode repository index state: %w", err)
	}
	for _, state := range saved.States {
		if state.Repository == "" || state.Status == "" {
			return fmt.Errorf("decode repository index state: invalid state")
		}
		s.states[state.Repository] = state
	}
	return nil
}

func (s *Store) saveLocked(next State) error {
	states := make([]State, 0, len(s.states)+1)
	for repository, state := range s.states {
		if repository != next.Repository {
			states = append(states, state)
		}
	}
	states = append(states, next)
	sort.Slice(states, func(i, j int) bool { return states[i].Repository < states[j].Repository })
	data, err := json.MarshalIndent(persisted{States: states}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode repository index state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".repository-index-*.tmp")
	if err != nil {
		return fmt.Errorf("create repository index state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write repository index state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set repository index state permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync repository index state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close repository index state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace repository index state: %w", err)
	}
	return nil
}
