package tailscale

import (
	"path/filepath"
	"testing"
)

// FuzzCertDomainPath checks that a certificate domain tslink accepts names
// files inside the certificate directory only.
//
// Guards: G6
func FuzzCertDomainPath(f *testing.F) {
	for _, d := range []string{"svc.example.ts.net", "../x.ts.net", "a/b.ts.net", "x", ".ts.net", "a..b.ts.net"} {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, domain string) {
		if !dnsName.MatchString(domain) {
			return
		}
		const dir = "/shared/certs"
		for _, ext := range []string{".crt", ".key", ".lease"} {
			p := filepath.Join(dir, domain+ext)
			if filepath.Dir(p) != dir || filepath.Base(p) != domain+ext {
				t.Fatalf("domain %q names %q, outside %s", domain, p, dir)
			}
		}
	})
}
