package tailscale

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aaomidi/tslink/pkg/logger"
)

// Every tailscaled keeps its certificates in one directory, on a volume shared
// between hosts if the plugin's shared mount points at one, so a Tailscale
// Service's certificate outlives its ephemeral nodes. tailscaled issues a
// certificate for a name it has none for, so without coordination every
// replica of a new Service would issue its own and exhaust Let's Encrypt's
// limit of five per name a week. Until a certificate exists, only the replica
// holding a lease configures the Service; the others wait for it.

const (
	certsDirName       = "certs"                // tailscaled's directory in its state directory
	acmeAccountKeyFile = "acme-account.key.pem" // tailscaled's name for its ACME account key
	certLeaseRefresh   = 15 * time.Second
	certLeaseStale     = 2 * time.Minute // tailscaled gives up on an issuance after 2 minutes
	certPollInterval   = 5 * time.Second
	protoHTTPS         = "https"
)

// errLeaseHeld means another replica holds the lease to issue a certificate.
var errLeaseHeld = errors.New("another replica is issuing the certificate")

// LinkCertsDir makes the state directory's certs directory a link to shared,
// creating shared and an ACME account key in it. A certs directory that is not
// a link, as kept by a non-ephemeral node from before, is left alone. An empty
// shared leaves tailscaled its own directory.
func LinkCertsDir(stateDir, shared string) error {
	if shared == "" {
		return nil
	}
	if err := os.MkdirAll(shared, 0700); err != nil {
		return fmt.Errorf("failed to create certificate directory: %w", err)
	}
	// Two tailscaleds would each create their own key, and certificates issued
	// under the one overwritten could no longer be renewed as renewals
	if err := ensureAccountKey(shared); err != nil {
		return err
	}

	link := filepath.Join(stateDir, certsDirName)
	switch st, err := os.Lstat(link); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("failed to check %s: %w", link, err)
	case st.Mode()&fs.ModeSymlink == 0:
		logger.Warn("%s is a directory, not shared: keeping it", link)
		return nil
	default:
		if target, err := os.Readlink(link); err == nil && target == shared {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("failed to replace %s: %w", link, err)
		}
	}
	if err := os.Symlink(shared, link); err != nil {
		return fmt.Errorf("failed to link %s: %w", link, err)
	}
	return nil
}

// ensureAccountKey creates an ACME account key in dir unless one exists. It is
// written to a temporary file and linked into place, so no tailscaled reads it
// half-written, and only one of several racing writers wins.
func ensureAccountKey(dir string) error {
	path := filepath.Join(dir, acmeAccountKeyFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate ACME account key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("failed to encode ACME account key: %w", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := createExclusive(path, pemKey, 0600); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("failed to write ACME account key: %w", err)
	}
	return nil
}

// createExclusive writes data to path unless path exists, atomically: it
// fails with fs.ErrExist if another writer got there first.
func createExclusive(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("Failed to remove %s: %v", tmp.Name(), err)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // the write error is the one to report
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close() // the chmod error is the one to report
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

// validCert reports whether dir holds an unexpired certificate for domain
// with its key, in tailscaled's file names. Whether it chains to a trusted
// root is left to tailscaled, which issues a new one if not.
func validCert(dir, domain string, now time.Time) bool {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, domain+".crt"), filepath.Join(dir, domain+".key"))
	if err != nil || pair.Leaf == nil {
		return false
	}
	return now.Before(pair.Leaf.NotAfter) && pair.Leaf.VerifyHostname(domain) == nil
}

// certLease is held by the replica issuing a domain's certificate. Its file's
// modification time is refreshed while held, so a lease of a replica that
// died goes stale.
type certLease struct {
	path string
	stop chan struct{}
	done chan struct{}
}

// tryLease takes the lease for domain in dir. It returns errLeaseHeld if
// another replica holds it and it is not stale.
func tryLease(dir, domain, holder string) (*certLease, error) {
	path := filepath.Join(dir, domain+".lease")
	err := createExclusive(path, []byte(holder+"\n"), 0600)
	if errors.Is(err, fs.ErrExist) {
		st, statErr := os.Stat(path)
		if statErr != nil || time.Since(st.ModTime()) < certLeaseStale {
			return nil, errLeaseHeld
		}
		// Two replicas may both find it stale and both take it; that costs
		// one certificate, not the limit
		logger.Warn("Taking over stale certificate lease %s", path)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("failed to remove stale lease: %w", err)
		}
		err = createExclusive(path, []byte(holder+"\n"), 0600)
		if errors.Is(err, fs.ErrExist) {
			return nil, errLeaseHeld
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to take certificate lease: %w", err)
	}

	l := &certLease{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	go l.refresh()
	return l, nil
}

func (l *certLease) refresh() {
	defer close(l.done)
	ticker := time.NewTicker(certLeaseRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			if err := os.Chtimes(l.path, now, now); err != nil {
				logger.Warn("Failed to refresh certificate lease %s: %v", l.path, err)
			}
		}
	}
}

// release gives up the lease.
func (l *certLease) release() {
	close(l.stop)
	<-l.done
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("Failed to release certificate lease %s: %v", l.path, err)
	}
}

// awaitCert returns once dir holds a valid certificate for domain.
func awaitCert(ctx context.Context, dir, domain string) error {
	for !validCert(dir, domain, time.Now()) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(certPollInterval):
		}
	}
	return nil
}

// configureServiceWhenCertified configures the Service backend, at once if it
// serves no HTTPS or its certificate exists, and otherwise in the background
// once it does or this replica holds the lease to issue it: waiting here would
// hold up the daemon's start.
func (d *Daemon) configureServiceWhenCertified() error {
	if !servesHTTPS(d.config.Endpoints) {
		return d.configureService()
	}
	domain, err := d.serviceDomain()
	if err != nil {
		return err
	}
	dir := filepath.Join(d.config.StateDir, certsDirName)
	if validCert(dir, domain, time.Now()) {
		return d.configureService()
	}
	logger.Info("No certificate for %s yet: configuring %s once there is one", domain, d.config.Service)
	go d.configureServiceAfterCert(dir, domain)
	return nil
}

func (d *Daemon) configureServiceAfterCert(dir, domain string) {
	for {
		err := d.configureServiceWithCert(dir, domain)
		if err == nil || d.ctx.Err() != nil {
			return
		}
		if !errors.Is(err, errLeaseHeld) {
			logger.Warn("Failed to configure %s, retrying: %v", d.config.Service, err)
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(certPollInterval):
		}
	}
}

// configureServiceWithCert configures the Service if its certificate exists,
// or if this replica takes the lease to issue it. It returns errLeaseHeld if
// another replica holds the lease.
func (d *Daemon) configureServiceWithCert(dir, domain string) error {
	if validCert(dir, domain, time.Now()) {
		if err := d.configureService(); err != nil {
			return err
		}
		logger.Info("Configured %s with the certificate for %s", d.config.Service, domain)
		return nil
	}

	lease, err := tryLease(dir, domain, d.config.EndpointID)
	if err != nil {
		return err
	}
	defer lease.release()
	logger.Info("Holding the certificate lease for %s: configuring %s to issue it", domain, d.config.Service)
	if err := d.configureService(); err != nil {
		return err
	}
	// Keep the lease until the certificate exists, so the others wait for it
	if err := awaitCert(d.ctx, dir, domain); err != nil {
		return err
	}
	logger.Info("Certificate for %s issued", domain)
	return nil
}

// servesHTTPS reports whether any endpoint makes tailscaled terminate TLS.
func servesHTTPS(endpoints []ServeEndpoint) bool {
	for _, ep := range endpoints {
		if ep.Proto == protoHTTPS {
			return true
		}
	}
	return false
}

// serviceDomain returns the Service's DNS name, which its certificate is for.
func (d *Daemon) serviceDomain() (string, error) {
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, d.config.TailscaleBin, "--socket="+d.socketPath, "status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status failed: %w", err)
	}
	var status struct {
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("failed to parse status: %w", err)
	}
	if status.MagicDNSSuffix == "" {
		return "", errors.New("tailscale status has no MagicDNS suffix: HTTPS needs MagicDNS")
	}
	return strings.TrimPrefix(d.config.Service, "svc:") + "." + status.MagicDNSSuffix, nil
}
