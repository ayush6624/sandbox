// Package management contains the security boundary shared by management
// listeners. It deliberately has no dependency on the worker or gateway so
// both processes enforce the same credential and transport rules.
package management

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials is a rotatable set of bearer credentials. Tokens from File are
// reloaded after an atomic file replacement; the first non-empty line is used
// for outbound calls and every line remains valid for inbound authentication.
// Keeping old+new in the file permits a no-downtime two-phase rotation.
type Credentials struct {
	static []string
	file   string

	mu      sync.RWMutex
	modTime time.Time
	size    int64
	fileSet []string
}

func NewCredentials(static []string, file string) (*Credentials, error) {
	c := &Credentials{static: cleanTokens(static), file: strings.TrimSpace(file)}
	if c.file != "" {
		if err := c.reload(true); err != nil {
			return nil, err
		}
	}
	if len(c.static) == 0 && len(c.fileSet) == 0 {
		return nil, errors.New("at least one management credential is required")
	}
	return c, nil
}

func cleanTokens(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func (c *Credentials) reload(required bool) error {
	info, err := os.Stat(c.file)
	if err != nil {
		if required {
			return fmt.Errorf("stat credential file %s: %w", c.file, err)
		}
		return nil // retain the last known-good set during an atomic rotation
	}
	c.mu.RLock()
	unchanged := info.ModTime().Equal(c.modTime) && info.Size() == c.size
	c.mu.RUnlock()
	if unchanged {
		return nil
	}
	f, err := os.Open(c.file)
	if err != nil {
		if required {
			return fmt.Errorf("open credential file %s: %w", c.file, err)
		}
		return nil
	}
	defer f.Close()
	var tokens []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens = append(tokens, line)
	}
	if err := scanner.Err(); err != nil {
		if required {
			return fmt.Errorf("read credential file %s: %w", c.file, err)
		}
		return nil
	}
	tokens = cleanTokens(tokens)
	if len(tokens) == 0 {
		if required {
			return fmt.Errorf("credential file %s contains no credentials", c.file)
		}
		return nil
	}
	c.mu.Lock()
	c.fileSet = tokens
	c.modTime = info.ModTime()
	c.size = info.Size()
	c.mu.Unlock()
	return nil
}

func (c *Credentials) tokens() []string {
	if c.file != "" {
		_ = c.reload(false)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.static)+len(c.fileSet))
	out = append(out, c.static...)
	out = append(out, c.fileSet...)
	return out
}

// MatchAuthorization checks a standard Authorization: Bearer header in
// constant time against every active credential.
func (c *Credentials) MatchAuthorization(header string) bool {
	got := sha256.Sum256([]byte(header))
	matched := 0
	for _, token := range c.tokens() {
		want := sha256.Sum256([]byte("Bearer " + token))
		matched |= subtle.ConstantTimeCompare(want[:], got[:])
	}
	return matched == 1
}

// Outbound returns the preferred credential for a control-plane request.
func (c *Credentials) Outbound() string {
	tokens := c.tokens()
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func (c *Credentials) Handler(next http.Handler, reject func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.MatchAuthorization(r.Header.Get("Authorization")) {
			reject(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
