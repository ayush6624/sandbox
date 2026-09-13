package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestChunkCacheBudgetConfig(t *testing.T) {
	for _, n := range []int64{-2, -1, 0, 1, 4096, 1 << 44} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"uffd_chunk_cache_mib":%d}`, n)), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			valid := n >= -1 && n <= (1<<63-1)>>20
			if valid && (err != nil || c.UFFDChunkCacheMIB != n) {
				t.Fatalf("budget %d: config=%+v err=%v", n, c, err)
			}
			if !valid && err == nil {
				t.Fatalf("invalid budget %d accepted", n)
			}
		})
	}
}
