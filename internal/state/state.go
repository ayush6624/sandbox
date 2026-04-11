package state

import (
	"encoding/json"
	"errors"
	"os"
)

// VMState persists information about a running VM so it can be stopped later.
type VMState struct {
	SocketPath string `json:"socket_path"`
	VMID       string `json:"vmid"`
}

// Save writes the state to path as JSON.
func Save(path string, s VMState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// Load reads the state from path.
func Load(path string) (VMState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return VMState{}, err
	}
	var s VMState
	if err := json.Unmarshal(b, &s); err != nil {
		return VMState{}, err
	}
	return s, nil
}

// Remove deletes the state file.
func Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
