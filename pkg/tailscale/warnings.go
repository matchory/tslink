package tailscale

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/matchory/tslink/pkg/logger"
)

// Every replica's tailscaled renews a Service's shared certificate on its
// own. While one replica's renewal order is pending, Let's Encrypt refuses
// the others' with alreadyReplaced, until that order completes or expires (7
// days at Let's Encrypt). Refused orders count against no rate limit, and
// with 90-day certificates renewed 30 days before expiry this is harmless,
// but a renewal blocked for long must be visible: tslink warns about blocked
// renewals, and about a shared certificate close to expiry.

// tailscaled logs certificate work prefixed with cert("<domain>"): (cert.go in
// tailscale.com/feature/acme); an ACME error reads "<status> <problem type>:
// <detail>" (tempfork/acme), and an issued certificate "got cert".
const (
	renewalBlockedProblem = "urn:ietf:params:acme:error:alreadyReplaced"
	certIssuedLog         = `"): got cert`
)

var certLogDomain = regexp.MustCompile(`cert\("([^"]+)"\): `)

// How often the node's log repeats a warning per domain, how close to expiry
// a certificate is warned about, and how often that is checked. Variables for
// tests.
var (
	renewalWarnEvery    = time.Hour
	certExpiryWarnEvery = 24 * time.Hour
	certExpiryMargin    = 14 * 24 * time.Hour
	certCheckInterval   = time.Hour
)

// warnLimiter lets a warning through once per period and key.
type warnLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether the warning key may be logged at now, and if so
// notes that it was.
func (l *warnLimiter) allow(key string, every time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[key]; ok && now.Sub(last) < every {
		return false
	}
	if l.last == nil {
		l.last = make(map[string]time.Time)
	}
	l.last[key] = now
	return true
}

// nodeWarnings limits the plugin log's warnings per domain: every replica on
// the node sees the same certificate.
var nodeWarnings = &warnLimiter{}

// warn reports a warning to the endpoint's status, and logs it unless logged
// within every. An empty message clears the warning.
func (d *Daemon) warn(key, message string, every time.Duration) {
	if message != "" && nodeWarnings.allow(key, every, time.Now()) {
		logger.Warnf("%s", message)
	}
	if d.config.Warn != nil {
		d.config.Warn(key, message)
	}
}

// handleCertLine notes a refused or a successful certificate renewal in a
// line of tailscaled's output.
func (d *Daemon) handleCertLine(line string) {
	m := certLogDomain.FindStringSubmatch(line)
	if m == nil {
		return
	}
	domain, key := m[1], "renewal-blocked/"+m[1]
	switch {
	case strings.Contains(line, renewalBlockedProblem):
		d.warn(key, fmt.Sprintf(
			"Renewing the certificate for %s is blocked: another replica's renewal order is "+
				"pending (ACME alreadyReplaced). Renewals resume once it completes or expires, "+
				"within 7 days at Let's Encrypt; until then the current certificate stays in use",
			domain,
		), renewalWarnEvery)
	case strings.Contains(line, certIssuedLog):
		d.warn(key, "", 0)
	}
}

// watchCertExpiry checks the certificate for domain in dir at once and then
// every certCheckInterval, until the daemon stops.
func (d *Daemon) watchCertExpiry(dir, domain string) {
	for {
		d.checkCertExpiry(dir, domain, time.Now())
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(certCheckInterval):
		}
	}
}

// checkCertExpiry warns if the certificate for domain in dir expires within
// certExpiryMargin of now, and clears the warning otherwise. Without a
// certificate, there is nothing to check.
func (d *Daemon) checkCertExpiry(dir, domain string, now time.Time) {
	notAfter, err := CertNotAfter(filepath.Join(dir, domain+".crt"))
	if err != nil {
		return
	}
	key := "cert-expiry/" + domain
	if notAfter.Sub(now) >= certExpiryMargin {
		d.warn(key, "", 0)
		return
	}
	d.warn(key, fmt.Sprintf(
		"The certificate for %s expires %s, in less than %d days: its renewals are failing "+
			"or blocked (see tailscaled.log)",
		domain, notAfter.UTC().Format(time.DateOnly), int(certExpiryMargin.Hours()/24),
	), certExpiryWarnEvery)
}

// CertNotAfter returns the expiry of the first certificate in a PEM file.
func CertNotAfter(path string) (time.Time, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- a file in the certificate directory
	if err != nil {
		return time.Time{}, err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return time.Time{}, errors.New("no certificate")
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return time.Time{}, err
			}
			return cert.NotAfter, nil
		}
	}
}
