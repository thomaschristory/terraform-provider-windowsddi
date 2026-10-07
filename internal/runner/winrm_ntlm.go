// NTLM transport for WinRM, used for both HTTP and HTTPS.
//
// Why not the library's own NTLM clients? NTLM authenticates a TCP
// connection, not a request: the negotiate, challenge and authenticate
// messages, and (over HTTP) the sealed requests that follow, must all travel
// on the same connection. The library's clients share one http.Transport (and
// its idle connection pool) between all calls, and one command makes several
// calls at once (stdin and output polling), as do Terraform's parallel
// resources. A handshake can then be split over two connections, or a request
// can go out on a pooled connection the server already closed. The symptoms
// are intermittent and different each time: "EOF", "401 - invalid content
// type", empty output, or a call that never returns.
//
// ntlmPerCall removes the sharing: each Post builds its own http.Transport and
// NTLM session, runs the whole exchange on one connection, and closes it.

package runner

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bodgit/ntlmssp"
	ntlmhttp "github.com/bodgit/ntlmssp/http"
	"github.com/masterzen/winrm"
	"github.com/masterzen/winrm/soap"
)

const (
	soapContentType = "application/soap+xml;charset=UTF-8"

	// WS-Management message encryption (MS-WSMV 2.2.9.1), same wire format as
	// the library's winrm.Encryption.
	sealBoundary    = "--Encrypted Boundary"
	sealProtocol    = "application/HTTP-SPNEGO-session-encrypted"
	sealContentType = `multipart/encrypted;protocol="` + sealProtocol + `";boundary="Encrypted Boundary"`
)

// ntlmPerCall implements winrm.Transporter (the Transport and Post methods).
// It holds only read-only settings, so it is safe to share between concurrent
// Posts. With encrypt set (plain HTTP) the SOAP messages are sealed with the
// NTLM session key; otherwise (HTTPS) TLS protects them.
type ntlmPerCall struct {
	user, password string
	encrypt        bool

	endpoint *winrm.Endpoint
	// proxy, when set, replaces http.ProxyFromEnvironment. Tests use it to
	// observe every request.
	proxy func(*http.Request) (*url.URL, error)
}

// Transport is called once by the library with the endpoint.
func (t *ntlmPerCall) Transport(endpoint *winrm.Endpoint) error {
	t.endpoint = endpoint
	return nil
}

// Post sends one SOAP message and returns the response body.
func (t *ntlmPerCall) Post(_ *winrm.Client, request *soap.SoapMessage) (string, error) {
	proxy := t.proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	// A transport of its own: its pool holds only this call's connection.
	//nolint:gosec // Insecure is the user's explicit insecure = true.
	tr := &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: t.endpoint.Insecure, ServerName: t.endpoint.TLSServerName},
		ResponseHeaderTimeout: t.endpoint.Timeout,
	}
	defer tr.CloseIdleConnections()

	domain, user := splitNTLMUser(t.user)
	nc, err := ntlmssp.NewClient(ntlmssp.SetUserInfo(user, t.password), ntlmssp.SetDomain(domain), ntlmssp.SetVersion(ntlmssp.DefaultVersion()))
	if err != nil {
		return "", fmt.Errorf("ntlm: %w", err)
	}
	// Encryption is not enabled here: it would replace the body without
	// updating the request's Content-Length. This client only handles the
	// handshake; sealing is done below.
	hc, err := ntlmhttp.NewClient(&http.Client{Transport: tr}, nc)
	if err != nil {
		return "", fmt.Errorf("ntlm: %w", err)
	}

	endpoint := t.url()

	// Step 1: authenticate with an empty request, so no message body is
	// involved in the 401 exchanges. This leaves the NTLM session (and, over
	// HTTP, its keys) established on the connection.
	if _, err := t.exchange(hc, endpoint, soapContentType, nil, nil); err != nil {
		return "", fmt.Errorf("ntlm handshake: %w", err)
	}

	// Step 2 over HTTPS: TLS protects the message, send it as is.
	if !t.encrypt {
		return t.exchange(hc, endpoint, soapContentType, strings.NewReader(request.String()), nil)
	}

	// Step 2 over HTTP: send the sealed message on the same connection and
	// unseal the reply. There is deliberately no fallback to an unencrypted
	// message.
	session := nc.SecuritySession()
	if session == nil {
		return "", errors.New("ntlm handshake did not produce a security session; refusing to send the message unencrypted")
	}
	payload, err := seal(session, []byte(request.String()))
	if err != nil {
		return "", err
	}
	return t.exchange(hc, endpoint, sealContentType, bytes.NewReader(payload), session)
}

// url returns the WS-Management endpoint URL.
func (t *ntlmPerCall) url() string {
	scheme := "http"
	if t.endpoint.HTTPS {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/wsman", scheme, net.JoinHostPort(t.endpoint.Host, strconv.Itoa(t.endpoint.Port)))
}

// exchange POSTs body and returns the response body. When session is not nil
// a sealed response is unsealed with it. Like the library, a SOAP fault that
// comes back with an error status is returned as the body so winrm can parse
// it; any other non-200 answer is an error.
func (t *ntlmPerCall) exchange(hc *ntlmhttp.Client, endpoint, contentType string, body io.Reader, session *ntlmssp.SecuritySession) (string, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, body) //nolint:noctx
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "WinRM client")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connection", "Keep-Alive")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("unknown error %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error while reading response body: %w", err)
	}

	if session != nil && strings.Contains(resp.Header.Get("Content-Type"), `protocol="`+sealProtocol+`"`) {
		plain, err := unseal(session, data)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http error %d: %s", resp.StatusCode, data)
	}
	if session == nil && body != nil && !strings.Contains(resp.Header.Get("Content-Type"), "application/soap+xml") {
		return "", fmt.Errorf("http response error: %d - invalid content type", resp.StatusCode)
	}
	return string(data), nil
}

// splitNTLMUser splits `DOMAIN\user` and `user@domain` into domain and user,
// like the library does. A bare name has an empty domain.
func splitNTLMUser(u string) (domain, user string) {
	if d, n, ok := strings.Cut(u, `\`); ok {
		return d, n
	}
	if n, d, ok := strings.Cut(u, "@"); ok {
		return d, n
	}
	return "", u
}

// seal builds the multipart/encrypted request body for one SOAP message: the
// message sealed with the NTLM session, preceded by its signature length and
// signature.
func seal(session *ntlmssp.SecuritySession, message []byte) ([]byte, error) {
	sealed, signature, err := session.Wrap(message)
	if err != nil {
		return nil, fmt.Errorf("sealing message: %w", err)
	}
	var b bytes.Buffer
	b.WriteString(sealBoundary + "\r\n")
	b.WriteString("\tContent-Type: " + sealProtocol + "\r\n")
	fmt.Fprintf(&b, "\tOriginalContent: type=%s;Length=%d\r\n", soapContentType, len(message))
	b.WriteString(sealBoundary + "\r\n")
	b.WriteString("\tContent-Type: application/octet-stream\r\n")
	// An NTLM signature is 16 bytes, so the conversion cannot overflow, and
	// writes to a bytes.Buffer cannot fail.
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(signature))) //nolint:gosec
	b.Write(signature)
	b.Write(sealed)
	b.WriteString(sealBoundary + "--\r\n")
	return b.Bytes(), nil
}

// unseal reverses seal for a response. Every length is checked before use, so
// a malformed or truncated reply is an error and never a panic.
func unseal(session *ntlmssp.SecuritySession, body []byte) ([]byte, error) {
	var parts [][]byte
	for _, p := range bytes.Split(body, []byte(sealBoundary+"\r\n")) {
		if len(p) != 0 {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 || len(parts)%2 != 0 {
		return nil, errors.New("malformed encrypted response")
	}
	var message []byte
	for i := 0; i < len(parts); i += 2 {
		header, payload := parts[i], parts[i+1]

		_, lengthText, ok := bytes.Cut(header, []byte("Length="))
		if !ok {
			return nil, errors.New("malformed encrypted response: no Length")
		}
		want, err := strconv.Atoi(string(bytes.TrimSpace(lengthText)))
		if err != nil {
			return nil, fmt.Errorf("malformed encrypted response: %w", err)
		}

		payload = bytes.TrimSuffix(payload, []byte(sealBoundary+"--\r\n"))
		payload = bytes.TrimPrefix(payload, []byte("\tContent-Type: application/octet-stream\r\n"))
		if len(payload) < 4 {
			return nil, errors.New("malformed encrypted response: truncated")
		}
		sigLen := int(binary.LittleEndian.Uint32(payload[:4]))
		if sigLen < 0 || sigLen > len(payload)-4 {
			return nil, errors.New("malformed encrypted response: bad signature length")
		}
		signature, sealed := payload[4:4+sigLen], payload[4+sigLen:]

		plain, err := session.Unwrap(sealed, signature)
		if err != nil {
			return nil, fmt.Errorf("unsealing response: %w", err)
		}
		if len(plain) != want {
			return nil, errors.New("encrypted length from server does not match the expected size, message has been tampered with")
		}
		message = append(message, plain...)
	}
	return message, nil
}
