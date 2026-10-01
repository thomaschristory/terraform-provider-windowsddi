// SSH transport: runs the PowerShell bootstrap through the OpenSSH server on
// Windows. One TCP/SSH connection is opened on first use and reused; each
// script runs in its own SSH session (channel) on that connection, which is
// much cheaper than a new handshake per script.

package runner

// Go note: golang.org/x/crypto/ssh is the Go team's SSH client/server
// library. It is pure Go, so no local ssh binary is involved.
import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHConfig configures the SSH transport. The provider fills it from the
// provider block (host, port, username, password, private_key, ssh_host_key,
// known_hosts_file, insecure).
//
// Password is the account password, and is also tried as the private key
// passphrase when PrivateKey is encrypted. Timeout bounds the TCP connect
// and defaults to 30 seconds.
//
// Go note: a struct's zero value has every field empty ("" / 0 / false), so
// unset options are simply left out when building one.
type SSHConfig struct {
	Host       string
	Port       int
	Username   string
	Password   string
	PrivateKey string // PEM encoded
	// HostKey pins the server host key (authorized_keys format). When empty,
	// KnownHostsFile is used.
	HostKey        string
	KnownHostsFile string
	// Insecure disables host key verification.
	Insecure bool
	Timeout  time.Duration
}

// SSH runs scripts over an SSH connection that is opened lazily and reused;
// every script gets its own session.
//
// Go note: "client *ssh.ClientConfig" is a pointer field. "sync.Mutex" is a
// lock: mu.Lock() / mu.Unlock() make sure only one goroutine (Go's
// lightweight thread) at a time reads or replaces conn.
type SSH struct {
	cfg    SSHConfig
	client *ssh.ClientConfig
	addr   string
	// hostKeyAlgos returns the host key algorithms to offer for a
	// connection, so the server presents a key type we can verify. nil means
	// the library default.
	//
	// Why: a Windows OpenSSH server usually has RSA, ECDSA and ed25519 host
	// keys. The client and server agree on one type during the handshake.
	// If the client prefers ed25519 but only the RSA key is pinned or in
	// known_hosts, verification fails even though the host is genuine.
	// Restricting the offered algorithms to the types we can check avoids
	// that false "host key mismatch".
	hostKeyAlgos func(remote net.Addr) []string

	// mu guards conn, the cached connection shared by concurrent Runs.
	mu   sync.Mutex
	conn *ssh.Client
}

// NewSSH validates cfg and returns an SSH runner. It does not connect; the
// first Run does. That keeps provider configuration fast and lets
// `terraform validate` work without network access.
//
// Go note: the return type (*SSH, error) means "a pointer to an SSH value,
// or an error". By convention exactly one of them is meaningful.
func NewSSH(cfg SSHConfig) (*SSH, error) {
	// Step 1: required fields and defaults.
	if cfg.Host == "" || cfg.Username == "" {
		return nil, errors.New("ssh: host and username are required")
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	// Step 2: build the list of authentication methods, tried in order:
	// public key first (if a key is given), then password and
	// keyboard-interactive.
	// Go note: "var auth []ssh.AuthMethod" declares an empty (nil) slice;
	// append adds items to it and returns the grown slice.
	var auth []ssh.AuthMethod
	if cfg.PrivateKey != "" {
		var (
			signer ssh.Signer
			err    error
		)
		if cfg.Password != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(cfg.PrivateKey), []byte(cfg.Password))
			if err != nil {
				// The password may be the account password, not a passphrase.
				signer, err = ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
			}
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("ssh: parsing private_key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		// Some Windows OpenSSH setups only offer keyboard-interactive
		// instead of plain password auth, so both are registered. The
		// keyboard-interactive callback answers every prompt with the
		// password.
		// Go note: the func(...) {...} passed below is an anonymous function
		// (closure); it can use pw from the surrounding code.
		pw := cfg.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}))
	}
	if len(auth) == 0 {
		return nil, errors.New("ssh: password or private_key is required")
	}
	// Step 3: decide how to verify the server's host key.
	hkc, algos, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}
	// Step 4: assemble the runner. net.JoinHostPort handles IPv6 literals
	// correctly ("[::1]:22").
	return &SSH{
		cfg:          cfg,
		addr:         net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		hostKeyAlgos: algos,
		client: &ssh.ClientConfig{
			User:            cfg.Username,
			Auth:            auth,
			HostKeyCallback: hkc,
			Timeout:         cfg.Timeout,
		},
	}, nil
}

// keyAlgorithms maps a public key type to the host key algorithms that can
// present it. An RSA key can be used with three signature algorithms
// (rsa-sha2-512, rsa-sha2-256 and the legacy SHA-1 ssh-rsa); the newer ones
// are listed first. Every other key type has exactly one algorithm.
func keyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}

// hostKeyCallback builds the host key check for cfg and, optionally, a
// function giving the host key algorithms to offer (see SSH.hostKeyAlgos).
//
// Three modes, in priority order:
//  1. insecure = true: accept any host key (explicit opt-in only).
//  2. ssh_host_key set: accept only that exact key.
//  3. otherwise: check against a known_hosts file (default
//     ~/.ssh/known_hosts), like the OpenSSH client does.
//
// Go note: this function returns three values; the second is itself a
// function. A nil function value means "not set".
func hostKeyCallback(cfg SSHConfig) (ssh.HostKeyCallback, func(net.Addr) []string, error) {
	// Mode 1: no verification.
	if cfg.Insecure {
		return ssh.InsecureIgnoreHostKey(), nil, nil //nolint:gosec // explicit opt-in via insecure = true
	}
	// Mode 2: pinned key, and offer only that key's algorithms.
	if cfg.HostKey != "" {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKey))
		if err != nil {
			return nil, nil, fmt.Errorf("ssh: parsing ssh_host_key: %w", err)
		}
		algos := keyAlgorithms(key.Type())
		return ssh.FixedHostKey(key), func(net.Addr) []string { return algos }, nil
	}
	// Mode 3: known_hosts. Step 1: locate and load the file.
	path := cfg.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("ssh: locating known_hosts: %w", err)
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, nil, fmt.Errorf("ssh: reading known_hosts_file %s (set ssh_host_key, or insecure = true to skip verification): %w", path, err)
	}
	// Step 2: the name as it appears in known_hosts ("host" for port 22,
	// "[host]:port" otherwise).
	hostname := knownhosts.Normalize(net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	// Step 3: wrap the known_hosts check to give a clearer message when the
	// host is missing entirely. A KeyError with an empty Want list means
	// "no entry for this host"; a non-empty Want means "entry exists but the
	// key differs" (possible man-in-the-middle), which is passed through.
	// Go note: errors.As checks whether err (or anything it wraps) is a
	// *knownhosts.KeyError and, if so, stores it in ke.
	verify := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			return fmt.Errorf("host %s is not in %s; add it (ssh-keyscan), set ssh_host_key, or set insecure = true", hostname, path)
		}
		return err
	}
	// Offer only the key types known_hosts holds for this host: probing the
	// callback with a throwaway key returns them in KeyError.Want, and this
	// also works for hashed entries.
	//
	// (The knownhosts package has no "list keys for host" function, and
	// hashed entries cannot be read back by name, so this trick asks the
	// checker itself: a random key never matches, and the resulting
	// mismatch error lists the keys it expected.)
	algos := func(remote net.Addr) []string {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil
		}
		probe, err := ssh.NewPublicKey(priv.Public())
		if err != nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(cb(hostname, remote, probe), &ke) {
			return nil
		}
		// Collect the algorithms for each expected key, without duplicates.
		// Go note: map[string]bool{} is an empty map (a hashtable, like
		// @{} in PowerShell); seen[a] is false for missing keys.
		var out []string
		seen := map[string]bool{}
		for _, k := range ke.Want {
			for _, a := range keyAlgorithms(k.Key.Type()) {
				if !seen[a] {
					seen[a] = true
					out = append(out, a)
				}
			}
		}
		return out
	}
	return verify, algos, nil
}

// connect returns the cached SSH connection, opening it on first use. The
// mutex makes concurrent Runs share one connection instead of racing to
// open several.
func (s *SSH) connect(ctx context.Context) (*ssh.Client, error) {
	// Go note: Lock, then "defer Unlock" is the usual pattern: the lock is
	// released however the function returns.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	// Step 1: open the TCP connection, honouring ctx cancellation and the
	// configured timeout.
	d := net.Dialer{Timeout: s.cfg.Timeout}
	nc, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	// Step 2: copy the client config and restrict the host key algorithms
	// for this connection. "*s.client" dereferences the pointer, so conf is
	// a separate copy and the shared config is never modified.
	conf := *s.client
	if s.hostKeyAlgos != nil {
		conf.HostKeyAlgorithms = s.hostKeyAlgos(nc.RemoteAddr())
	}
	// Step 3: SSH handshake (key exchange, host key check, authentication).
	// ClientConfig.Timeout only covers dialing, so put a deadline on the
	// socket for the handshake too (and honour an earlier ctx deadline):
	// otherwise a server that stalls mid-handshake would hang every Run,
	// since they all wait on s.mu. The deadline is cleared afterwards
	// because the connection is reused for later commands.
	deadline := time.Now().Add(s.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = nc.SetDeadline(deadline)
	c, chans, reqs, err := ssh.NewClientConn(nc, s.addr, &conf)
	_ = nc.SetDeadline(time.Time{})
	if err != nil {
		// Go note: assigning to "_" explicitly ignores a return value (here
		// the error from Close, which we cannot do anything useful with).
		_ = nc.Close()
		return nil, err
	}
	s.conn = ssh.NewClient(c, chans, reqs)
	return s.conn, nil
}

// drop closes and forgets the cached connection, but only if it is still c.
// The check matters under concurrency: another Run may already have
// replaced a dead connection with a fresh one, which must not be closed.
func (s *SSH) drop(c *ssh.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == c {
		_ = s.conn.Close()
		s.conn = nil
	}
}

// Run implements Runner. "s.exec" passes the exec method (bound to s) as a
// function value to the shared execute helper.
func (s *SSH) Run(ctx context.Context, script string, params any) ([]byte, error) {
	return execute(ctx, "ssh", s.exec, script, params)
}

// exec is the SSH execFunc: it runs cmdline in a new session on the shared
// connection, feeds stdin, and collects stdout, stderr and the exit status.
func (s *SSH) exec(ctx context.Context, cmdline, stdin string) ([]byte, []byte, int, error) {
	// Step 1: get a connection and open a session (an SSH channel).
	conn, err := s.connect(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("connecting to %s: %w", s.addr, err)
	}
	sess, err := conn.NewSession()
	if err != nil {
		// The cached connection may have died; reconnect once.
		// (For example the server restarted or an idle timeout closed it.)
		s.drop(conn)
		// Go note: "=" (not ":=") assigns to the existing conn and err
		// variables instead of declaring new ones.
		if conn, err = s.connect(ctx); err != nil {
			return nil, nil, 0, fmt.Errorf("connecting to %s: %w", s.addr, err)
		}
		if sess, err = conn.NewSession(); err != nil {
			s.drop(conn)
			return nil, nil, 0, fmt.Errorf("opening ssh session: %w", err)
		}
	}
	defer func() { _ = sess.Close() }()

	// Step 2: wire stdin/stdout/stderr. bytes.Buffer is an in-memory
	// growable byte buffer that collects the output.
	var stdout, stderr bytes.Buffer
	sess.Stdin = strings.NewReader(stdin)
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// Step 3: run the command in the background so we can also watch ctx.
	// sess.Run blocks until the remote command exits and has no context
	// parameter, so cancellation is handled by closing the session.
	// Go note: "go f()" starts f in a new goroutine (runs concurrently).
	// The channel "done" (buffer of 1, so the goroutine never blocks even if
	// nobody reads) carries its result back.
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmdline) }()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		return nil, nil, 0, ctx.Err()
	case err := <-done:
		// Step 4: translate the result. A non-zero remote exit status comes
		// back as *ssh.ExitError; that is a normal result (the caller
		// inspects the code), not a transport failure.
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), stderr.Bytes(), exitErr.ExitStatus(), nil
		}
		if err != nil {
			return nil, nil, 0, err
		}
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
}
