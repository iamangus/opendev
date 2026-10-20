// Package dispatchstore provides durable JSON storage for AgentFoundry dispatch runs.
package dispatchstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/iamangus/code-mcp/internal/dispatcher"
	"github.com/iamangus/code-mcp/internal/durable"
)

type persisted struct {
	Runs []dispatcher.DispatchRun `json:"runs"`
}

// Store is a restart-safe implementation of dispatcher.RunStore.
type Store struct {
	path string
	mu   sync.RWMutex
	runs map[string]dispatcher.DispatchRun
}

func New(stateDir string) (*Store, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("dispatch state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, fmt.Errorf("create dispatch state directory: %w", err)
	}
	s := &Store{path: filepath.Join(stateDir, "dispatch-runs.json"), runs: make(map[string]dispatcher.DispatchRun)}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Get(_ context.Context, taskID string) (*dispatcher.DispatchRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.runs[taskID]
	if !ok {
		parts := strings.SplitN(taskID, ":", 3)
		if len(parts) == 3 && parts[0] == "opendev" {
			runs, err := s.archived(parts[1])
			if err != nil {
				return nil, err
			}
			for _, run := range runs {
				if run.TaskID == taskID {
					return &run, nil
				}
			}
		}
		return nil, nil
	}
	return &run, nil
}

func (s *Store) Save(_ context.Context, run dispatcher.DispatchRun) error {
	if run.TaskID == "" {
		return fmt.Errorf("dispatch task ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[run.TaskID]; !exists {
		archived, err := s.archived(run.JobID)
		if err != nil {
			return err
		}
		for _, previous := range archived {
			if previous.TaskID == run.TaskID {
				return nil
			}
		}
	}
	previous, existed := s.runs[run.TaskID]
	s.runs[run.TaskID] = run
	if err := s.saveLocked(); err != nil {
		if existed {
			s.runs[run.TaskID] = previous
		} else {
			delete(s.runs, run.TaskID)
		}
		return err
	}
	return nil
}

func (s *Store) ListUnapplied(_ context.Context) ([]dispatcher.DispatchRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs := make([]dispatcher.DispatchRun, 0, len(s.runs))
	for _, run := range s.runs {
		if !isTerminal(run.Status) || !run.OutcomeApplied {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].TaskID < runs[j].TaskID })
	return runs, nil
}

// ListByJob returns every attempt, including applied and superseded results.
func (s *Store) ListByJob(_ context.Context, jobID string) ([]dispatcher.DispatchRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs, err := s.archived(jobID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, run := range runs {
		seen[run.TaskID] = true
	}
	for _, run := range s.runs {
		if run.JobID == jobID && !seen[run.TaskID] {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].TaskID < runs[j].TaskID })
	return runs, nil
}

func (s *Store) archived(id string) ([]dispatcher.DispatchRun, error) {
	path, err := durable.ArchivePath(s.path, "dispatch", id)
	if err != nil {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []dispatcher.DispatchRun
	err = json.Unmarshal(data, &runs)
	return runs, err
}

func (s *Store) ArchiveJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs, err := s.archived(id)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, run := range runs {
		seen[run.TaskID] = true
	}
	removed := make(map[string]dispatcher.DispatchRun)
	for key, run := range s.runs {
		if run.JobID == id {
			if !isTerminal(run.Status) || !run.OutcomeApplied {
				return fmt.Errorf("dispatch %s is still active or unapplied", key)
			}
			if !seen[key] {
				runs = append(runs, run)
			}
			removed[key] = run
		}
	}
	if len(removed) == 0 {
		return nil
	}
	path, err := durable.ArchivePath(s.path, "dispatch", id)
	if err != nil {
		return err
	}
	if err := durable.WriteJSON(path, runs); err != nil {
		return err
	}
	for key := range removed {
		delete(s.runs, key)
	}
	if err := s.saveLocked(); err != nil {
		for key, run := range removed {
			s.runs[key] = run
		}
		return err
	}
	return nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read dispatch state: %w", err)
	}
	var saved persisted
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("decode dispatch state: %w", err)
	}
	for _, run := range saved.Runs {
		if run.TaskID == "" {
			return fmt.Errorf("decode dispatch state: run without task ID")
		}
		s.runs[run.TaskID] = run
	}
	return nil
}

func (s *Store) saveLocked() error {
	runs := make([]dispatcher.DispatchRun, 0, len(s.runs))
	for _, run := range s.runs {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].TaskID < runs[j].TaskID })
	data, err := json.MarshalIndent(persisted{Runs: runs}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dispatch state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".dispatch-runs-*.tmp")
	if err != nil {
		return fmt.Errorf("create dispatch state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write dispatch state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set dispatch state permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync dispatch state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close dispatch state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace dispatch state: %w", err)
	}
	return durable.SyncDirectory(s.path)
}

func isTerminal(status string) bool {
	switch status {
	case "completed", "failed", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

var _ dispatcher.RunStore = (*Store)(nil)
