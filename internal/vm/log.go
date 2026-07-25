package vm

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// boundedLogFile consumes all process output but persists at most limit bytes.
// Returning the original input length after the cap is reached is important:
// Firecracker must never block or fail because its diagnostic sink is full.
type boundedLogFile struct {
	mu        sync.Mutex
	file      *os.File
	remaining int64
}

func openBoundedLog(path string, limit int64) (*boundedLogFile, error) {
	if limit <= 0 {
		limit = defaultLogMaxBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &boundedLogFile{file: f, remaining: limit}, nil
}

func (w *boundedLogFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	consumed := len(p)
	if w.file == nil || w.remaining <= 0 {
		return consumed, nil
	}
	toWrite := p
	if int64(len(toWrite)) > w.remaining {
		toWrite = toWrite[:w.remaining]
	}
	n, err := w.file.Write(toWrite)
	w.remaining -= int64(n)
	if err != nil {
		// Continue draining the child pipe even if the host log filesystem has
		// failed; the VMM lifecycle is more important than diagnostics.
		return consumed, nil
	}
	return consumed, nil
}

func (w *boundedLogFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

var _ io.WriteCloser = (*boundedLogFile)(nil)

// vmmLog owns one Firecracker diagnostic file. Expected lifecycle exits are
// deleted after the process has stopped; unexpected exits retain their capped
// file for debugging. Retained files are pruned by both age and count.
type vmmLog struct {
	*boundedLogFile
	path      string
	retention time.Duration
	maxFiles  int
	expected  atomic.Bool
	finish    sync.Once
}

var activeVMMLogs = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

func openVMMLog(path string, limit int64, retention time.Duration, maxFiles int) (*vmmLog, error) {
	if retention <= 0 {
		retention = defaultLogRetention
	}
	if maxFiles <= 0 {
		maxFiles = defaultLogMaxFiles
	}
	_ = pruneVMMLogs(filepath.Dir(path), retention, maxFiles, time.Now())
	f, err := openBoundedLog(path, limit)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	activeVMMLogs.Lock()
	activeVMMLogs.paths[path] = struct{}{}
	activeVMMLogs.Unlock()
	return &vmmLog{
		boundedLogFile: f,
		path:           path,
		retention:      retention,
		maxFiles:       maxFiles,
	}, nil
}

func (l *vmmLog) markExpectedExit() {
	if l != nil {
		l.expected.Store(true)
	}
}

// finishExit must run only after the VMM process has exited. Clean and
// explicitly requested exits remove their diagnostics immediately. A crash
// retains the bounded file for the configured diagnostic window.
func (l *vmmLog) finishExit(waitErr error) {
	if l == nil {
		return
	}
	l.finish.Do(func() {
		_ = l.boundedLogFile.Close()
		activeVMMLogs.Lock()
		delete(activeVMMLogs.paths, l.path)
		activeVMMLogs.Unlock()
		if waitErr == nil || l.expected.Load() {
			_ = os.Remove(l.path)
		}
		_ = pruneVMMLogs(filepath.Dir(l.path), l.retention, l.maxFiles, time.Now())
	})
}

// Close releases a log whose VMM never reached the normal wait path. The file
// is retained as a startup-failure diagnostic and remains subject to pruning.
func (l *vmmLog) Close() error {
	if l == nil {
		return nil
	}
	var err error
	l.finish.Do(func() {
		err = l.boundedLogFile.Close()
		activeVMMLogs.Lock()
		delete(activeVMMLogs.paths, l.path)
		activeVMMLogs.Unlock()
		_ = pruneVMMLogs(filepath.Dir(l.path), l.retention, l.maxFiles, time.Now())
	})
	return err
}

type retainedLog struct {
	path    string
	modTime time.Time
}

// pruneVMMLogs bounds aggregate retained diagnostics without touching logs
// owned by VMMs active in this process. Symlinks and unrelated files are
// ignored. Retained legacy files are tightened to mode 0600 during the sweep.
func pruneVMMLogs(dir string, retention time.Duration, maxFiles int, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if retention <= 0 {
		retention = defaultLogRetention
	}
	if maxFiles <= 0 {
		maxFiles = defaultLogMaxFiles
	}

	activeVMMLogs.Lock()
	active := make(map[string]struct{}, len(activeVMMLogs.paths))
	for path := range activeVMMLogs.paths {
		active[path] = struct{}{}
	}
	activeVMMLogs.Unlock()

	logs := make([]retainedLog, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "firecracker-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		path, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if _, ok := active[path]; ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		_ = os.Chmod(path, 0o600)
		logs = append(logs, retainedLog{path: path, modTime: info.ModTime()})
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].modTime.After(logs[j].modTime) })
	cutoff := now.Add(-retention)
	for i, log := range logs {
		if i >= maxFiles || log.modTime.Before(cutoff) {
			_ = os.Remove(log.path)
		}
	}
	return nil
}
