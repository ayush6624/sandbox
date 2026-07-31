package edge

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
)

type fileStamp struct {
	modUnixNano int64
	size        int64
}

type certificateReloader struct {
	certFile string
	keyFile  string
	met      *metrics

	mu        sync.Mutex
	cert      *tls.Certificate
	certStamp fileStamp
	keyStamp  fileStamp
	failed    [2]fileStamp
}

func newCertificateReloader(certFile, keyFile string, met *metrics) (*certificateReloader, error) {
	r := &certificateReloader{certFile: certFile, keyFile: keyFile, met: met}
	if err := r.reload(true); err != nil {
		return nil, err
	}
	return r, nil
}

func stamp(path string) (fileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{modUnixNano: info.ModTime().UnixNano(), size: info.Size()}, nil
}

func (r *certificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.reloadLocked(false); err != nil {
		// Keep serving the last known-good pair. A partially written or invalid
		// renewal must not take the public listener down.
		return r.cert, nil
	}
	return r.cert, nil
}

func (r *certificateReloader) reload(initial bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked(initial)
}

func (r *certificateReloader) reloadLocked(initial bool) error {
	cs, err := stamp(r.certFile)
	if err != nil {
		return err
	}
	ks, err := stamp(r.keyFile)
	if err != nil {
		return err
	}
	if r.cert != nil && cs == r.certStamp && ks == r.keyStamp {
		return nil
	}
	if !initial && r.failed == [2]fileStamp{cs, ks} {
		return fmt.Errorf("certificate files remain at a previously rejected version")
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		r.failed = [2]fileStamp{cs, ks}
		r.met.certReloadErr.Add(1)
		return err
	}
	if len(pair.Certificate) == 0 {
		return fmt.Errorf("certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		r.failed = [2]fileStamp{cs, ks}
		r.met.certReloadErr.Add(1)
		return err
	}
	pair.Leaf = leaf
	r.cert = &pair
	r.certStamp, r.keyStamp = cs, ks
	r.failed = [2]fileStamp{}
	r.met.certExpiryUnix.Store(leaf.NotAfter.Unix())
	if !initial {
		r.met.certReloadOK.Add(1)
	}
	return nil
}

var wakeBounds = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 15}

func (m *metrics) observeWake(seconds float64) {
	m.wakeCount.Add(1)
	m.wakeNanos.Add(int64(seconds * 1e9))
	for i, bound := range wakeBounds {
		if seconds <= bound {
			m.wakeBuckets[i].Add(1)
		}
	}
}
