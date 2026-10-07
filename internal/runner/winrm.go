// WinRM transport: runs the PowerShell bootstrap through Windows Remote
// Management (WS-Management over HTTP 5985 or HTTPS 5986), using the
// github.com/masterzen/winrm library. Each Run opens a fresh remote shell,
// runs one command and closes the shell.

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/masterzen/winrm"
)

// WinRM authentication types, as accepted by the provider's winrm_auth
// attribute.
//
// Go note: a const ( ... ) block groups related constants. Their type
// (string) is inferred from the values.
const (
	WinRMAuthNTLM     = "ntlm"
	WinRMAuthBasic    = "basic"
	WinRMAuthKerberos = "kerberos"
)

// WinRMConfig configures the WinRM transport. The provider fills it from
// the provider block (host, port, username, password, winrm_https,
// insecure, winrm_auth, kerberos_*).
//
// Insecure skips TLS certificate verification for HTTPS. KerberosSPN is
// optional; the library default is HTTP/<host>. Timeout bounds each WinRM
// operation and defaults to 60 seconds.
type WinRMConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	HTTPS    bool
	Insecure bool
	Auth     string // ntlm (default), basic or kerberos

	KerberosRealm  string
	KerberosConfig string // path to krb5.conf
	KerberosSPN    string
	Timeout        time.Duration
}

// WinRM runs scripts through WinRM, one shell per script. The underlying
// *winrm.Client holds only configuration (no open connection), so it is safe
// to share between concurrent Runs.
type WinRM struct {
	client *winrm.Client
}

// NewWinRM validates cfg and returns a WinRM runner. It does not connect.
//
// Its main job is choosing the transport that matches the auth mode:
//   - ntlm: ntlmPerCall (winrm_ntlm.go). Every call gets its own connection
//     and NTLM session, because NTLM authenticates a TCP connection and the
//     library's shared clients break when calls run concurrently. Over HTTPS
//     TLS protects the traffic; over HTTP the messages are sealed with the
//     NTLM session key, so the server can keep AllowUnencrypted = false,
//     which is the Windows default and the secure setting.
//   - basic: the library's default transport. Basic sends the password in
//     every request, so it should only be used over HTTPS (and the server
//     must enable Basic auth, local accounts only).
//   - kerberos: a gokrb5-based client that uses krb5.conf, the realm and
//     the SPN from the config.
func NewWinRM(cfg WinRMConfig) (*WinRM, error) {
	// Step 1: required fields and defaults (port 5985 for HTTP, 5986 for
	// HTTPS, 60s timeout, NTLM auth).
	if cfg.Host == "" || cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("winrm: host, username and password are required")
	}
	if cfg.Port == 0 {
		cfg.Port = 5985
		if cfg.HTTPS {
			cfg.Port = 5986
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Auth == "" {
		cfg.Auth = WinRMAuthNTLM
	}

	// Step 2: describe the endpoint and the WS-Management parameters. The
	// nil arguments are the optional CA certificate, client certificate and
	// client key, which this provider does not use.
	// The operation timeout uses the ISO 8601 duration format WS-Management
	// expects, for example "PT60S".
	// Go note: "*winrm.DefaultParameters" dereferences a package-level
	// pointer, so params is a private copy we can modify safely.
	endpoint := winrm.NewEndpoint(cfg.Host, cfg.Port, cfg.HTTPS, cfg.Insecure, nil, nil, nil, cfg.Timeout)
	params := *winrm.DefaultParameters
	params.Timeout = fmt.Sprintf("PT%dS", int(cfg.Timeout.Seconds()))
	user := cfg.Username

	// Step 3: pick the HTTP transport for the auth mode. TransportDecorator
	// is a factory function the library calls to create the transport.
	// Go note: Go's switch does not fall through between cases, so no
	// "break" is needed; "default" catches everything else.
	switch cfg.Auth {
	case WinRMAuthNTLM:
		// Over plain HTTP the SOAP messages are sealed (encrypt), so the
		// server does not need AllowUnencrypted.
		params.TransportDecorator = func() winrm.Transporter {
			return &ntlmPerCall{user: cfg.Username, password: cfg.Password, encrypt: !cfg.HTTPS}
		}
	case WinRMAuthBasic:
		// default transport
	case WinRMAuthKerberos:
		if cfg.KerberosRealm == "" {
			return nil, errors.New("winrm: kerberos_realm is required with winrm_auth = \"kerberos\"")
		}
		krbConf := cfg.KerberosConfig
		if krbConf == "" {
			krbConf = "/etc/krb5.conf"
		}
		proto := "http"
		if cfg.HTTPS {
			proto = "https"
		}
		// gokrb5 wants "svc" plus realm "EXAMPLE.LOCAL", not `EXAMPLE\svc`.
		krbUser := kerberosUser(cfg.Username)
		params.TransportDecorator = func() winrm.Transporter {
			return winrm.NewClientKerberos(&winrm.Settings{
				WinRMUsername: krbUser,
				WinRMPassword: cfg.Password,
				WinRMHost:     cfg.Host,
				WinRMPort:     cfg.Port,
				WinRMProto:    proto,
				WinRMInsecure: cfg.Insecure,
				KrbRealm:      cfg.KerberosRealm,
				KrbConfig:     krbConf,
				KrbSpn:        cfg.KerberosSPN,
			})
		}
	default:
		return nil, fmt.Errorf("winrm: unsupported winrm_auth %q (want ntlm, basic or kerberos)", cfg.Auth)
	}

	// Step 4: build the client (still no network traffic at this point).
	// Go note: "if err != nil { return ... }" is Go's error handling: check
	// the returned error after each call and stop early on failure.
	c, err := winrm.NewClientWithParameters(endpoint, user, cfg.Password, &params)
	if err != nil {
		return nil, fmt.Errorf("winrm: %w", err)
	}
	return &WinRM{client: c}, nil
}

// kerberosUser strips a NetBIOS domain prefix or a UPN suffix: gokrb5 wants
// the bare principal name plus the realm. For example `EXAMPLE\svc` and
// "svc@EXAMPLE.LOCAL" both become "svc".
// Go note: `\` below is a raw string literal (backticks), so the backslash
// is taken literally and needs no escaping.
func kerberosUser(u string) string {
	if _, after, ok := strings.Cut(u, `\`); ok {
		u = after
	}
	if before, _, ok := strings.Cut(u, "@"); ok {
		u = before
	}
	return u
}

// Run implements Runner by handing the WinRM exec function to the shared
// execute helper.
//
// Go note: "(w *WinRM)" is the method receiver: Run is a method on pointers
// to WinRM values, and w is how the method refers to its own value.
func (w *WinRM) Run(ctx context.Context, script string, params any) ([]byte, error) {
	return execute(ctx, "winrm", w.exec, script, params)
}

// exec is the WinRM execFunc. The library creates a remote shell, sends
// the command line, streams stdin, collects stdout/stderr and the exit code,
// and deletes the shell. ctx cancellation aborts the call.
// Go note: "&stdout" passes a pointer, so the library writes into our
// buffers rather than into copies.
func (w *WinRM) exec(ctx context.Context, cmdline, stdin string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	code, err := w.client.RunWithContextWithInput(ctx, cmdline, &stdout, &stderr, strings.NewReader(stdin))
	if err != nil {
		return nil, nil, 0, err
	}
	return stdout.Bytes(), stderr.Bytes(), code, nil
}
