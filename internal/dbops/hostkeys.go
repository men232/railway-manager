package dbops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/men232/railway-manager/internal/proc"
	"github.com/men232/railway-manager/internal/text"
	"github.com/men232/railway-manager/internal/workdir"
)

// KnownHostsFile is the tool's own known hosts file in the work dir: ssh
// trusts only the host keys the operator confirmed there, never ones it
// learns by itself or the user's ~/.ssh/known_hosts.
func KnownHostsFile(workDir string) string { return filepath.Join(workDir, "known_hosts") }

// HostKeys are the keys an ssh host presented, as ssh-keyscan read them:
// unauthenticated until the operator compares Fingerprints with the host's
// published ones and TrustHostKeys writes Keys.
type HostKeys struct {
	Host string `json:"host"`
	// Keys are known_hosts lines ("host type base64").
	Keys []string `json:"keys"`
	// Fingerprints are the keys' SHA256 fingerprints, as ssh-keygen -l
	// prints them.
	Fingerprints []string `json:"fingerprints"`
}

// UntrustedHostError: the known hosts file does not hold the host yet, so
// no tunnel was opened and nothing was sent to it.
type UntrustedHostError struct{ HostKeys }

func (e *UntrustedHostError) Error() string {
	fp := "no key read"
	if len(e.Fingerprints) > 0 {
		fp = strings.Join(e.Fingerprints, ", ")
	}
	return fmt.Sprintf("the ssh host key of %s is not trusted yet (%s): compare it with Railway's, then t trusts it", text.Sanitize(e.Host), fp)
}

// keyscanTimeout bounds ssh-keyscan.
const keyscanTimeout = 15 * time.Second

// Trusted reports whether file holds a key for host. A missing file holds
// none.
func Trusted(file, host string) (bool, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "@") {
			continue
		}
		if slices.Contains(strings.Split(f[0], ","), host) {
			return true, nil
		}
	}
	return false, sc.Err()
}

// ScanHostKeys reads host's keys with ssh-keyscan. Nothing is trusted: the
// keys come over an unauthenticated connection.
func ScanHostKeys(ctx context.Context, r proc.Runner, host string) (HostKeys, error) {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\n,") {
		return HostKeys{}, fmt.Errorf("bad ssh host %q", text.Sanitize(host))
	}
	ctx, cancel := context.WithTimeout(ctx, keyscanTimeout)
	defer cancel()
	out, _, err := r.Run(ctx, proc.Cmd{Name: "ssh-keyscan", Args: []string{"-T", "10", "-t", "ed25519,ecdsa,rsa", host}})
	if err != nil {
		return HostKeys{}, fmt.Errorf("read the ssh host key of %s: %w", text.Sanitize(host), err)
	}
	hk := HostKeys{Host: host}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != host {
			continue
		}
		fp, err := fingerprint(f[2])
		if err != nil {
			continue
		}
		hk.Keys = append(hk.Keys, host+" "+f[1]+" "+f[2])
		hk.Fingerprints = append(hk.Fingerprints, fp)
	}
	if len(hk.Keys) == 0 {
		return hk, fmt.Errorf("ssh-keyscan read no key from %s", text.Sanitize(host))
	}
	return hk, nil
}

// fingerprint is the SHA256 fingerprint of a base64 key blob, as
// ssh-keygen -l prints it.
func fingerprint(blob string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// TrustHostKeys adds hk's keys to file (0600), keeping what it holds.
func TrustHostKeys(file string, hk HostKeys) error {
	if len(hk.Keys) == 0 {
		return errors.New("no host key to trust")
	}
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	for _, k := range hk.Keys {
		if f := strings.Fields(k); len(f) != 3 || f[0] != hk.Host {
			return fmt.Errorf("bad host key line for %s", text.Sanitize(hk.Host))
		}
		data = append(data, k+"\n"...)
	}
	return workdir.WriteFileAtomic(file, data, 0o600)
}
