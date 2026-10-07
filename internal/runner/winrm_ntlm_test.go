// Tests for the NTLM transport (winrm_ntlm.go). A full NTLM exchange needs a
// Windows server (see docs/LAB_SETUP.md), so these cover what can be checked
// without one: user name parsing, rejection of malformed encrypted replies,
// how reply statuses and content types are handled, and the connection
// handling that fixes the intermittent EOF, 401 and hang errors.

package runner

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/masterzen/winrm"
	"github.com/masterzen/winrm/soap"
)

func TestSplitNTLMUser(t *testing.T) {
	for _, tc := range []struct{ in, domain, user string }{
		{`EXAMPLE\svc`, "EXAMPLE", "svc"},
		{"svc@example.local", "example.local", "svc"},
		{"svc", "", "svc"},
	} {
		if d, u := splitNTLMUser(tc.in); d != tc.domain || u != tc.user {
			t.Errorf("splitNTLMUser(%q) = %q, %q; want %q, %q", tc.in, d, u, tc.domain, tc.user)
		}
	}
}

// TestUnsealRejectsMalformed checks that bad replies are errors, not panics.
// Every case fails before the (nil) session would be used.
func TestUnsealRejectsMalformed(t *testing.T) {
	const b = sealBoundary + "\r\n"
	hdr := "\tContent-Type: " + sealProtocol + "\r\n\tOriginalContent: type=application/soap+xml;charset=UTF-8;Length=10\r\n"
	oct := "\tContent-Type: application/octet-stream\r\n"
	for name, body := range map[string]string{
		"empty":                              "",
		"no payload part":                    b + hdr,
		"no Length":                          b + "\tContent-Type: x\r\n" + b + oct + "abcd",
		"bad Length":                         b + "\tOriginalContent: Length=abc\r\n" + b + oct + "abcd",
		"payload too short":                  b + hdr + b + oct + "abc",
		"signature too big":                  b + hdr + b + oct + "\xff\xff\xff\x7fxx",
		"signature length huge (uint32 max)": b + hdr + b + oct + "\xff\xff\xff\xffxx",
	} {
		if _, err := unseal(nil, []byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestReply checks how statuses and content types become Post results. The
// "open" stub stands in for unsealing (a real NTLM session needs a server).
func TestReply(t *testing.T) {
	const (
		soapCT   = "application/soap+xml;charset=UTF-8"
		sealedCT = sealContentType
		fault    = "<s:Fault>The WS-Management service cannot complete the operation within the time specified in OperationTimeout.</s:Fault>"
	)
	open := func(b []byte) ([]byte, error) { return []byte("plain:" + string(b)), nil }
	failOpen := func([]byte) ([]byte, error) { return nil, errors.New("bad signature") }

	for _, tc := range []struct {
		name     string
		status   int
		ct       string
		data     string
		wantSOAP bool
		open     func([]byte) ([]byte, error)
		want     string // expected result when wantErr is empty
		wantErr  string // substring of the expected error
	}{
		{name: "handshake ok", status: 200, wantSOAP: false, want: ""},
		{name: "handshake 401", status: 401, wantErr: "http error 401"},
		{name: "plain ok", status: 200, ct: soapCT, data: "<x/>", wantSOAP: true, want: "<x/>"},
		{name: "plain wrong content type", status: 200, ct: "text/html", data: "<x/>", wantSOAP: true, wantErr: "invalid content type"},
		{name: "plain fault is an error", status: 500, ct: soapCT, data: fault, wantSOAP: true, wantErr: "OperationTimeout"},
		{name: "sealed ok", status: 200, ct: sealedCT, data: "x", wantSOAP: true, open: open, want: "plain:x"},
		// The bug this guards against: a sealed fault returned as a normal
		// body makes winrm poll for output forever.
		{name: "sealed fault is an error with the unsealed body", status: 500, ct: sealedCT, data: fault, wantSOAP: true, open: open, wantErr: "http error 500: plain:<s:Fault>The WS-Management service cannot complete the operation within the time specified in OperationTimeout"},
		{name: "sealed unseal failure", status: 200, ct: sealedCT, data: "x", wantSOAP: true, open: failOpen, wantErr: "bad signature"},
		{name: "unsealed 200 after sealed request is refused", status: 200, ct: soapCT, data: "<forged/>", wantSOAP: true, open: open, wantErr: "unencrypted reply"},
		{name: "unsealed error after sealed request", status: 401, ct: "", data: "", wantSOAP: true, open: open, wantErr: "http error 401"},
	} {
		got, err := reply(tc.status, tc.ct, []byte(tc.data), tc.wantSOAP, tc.open)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.wantErr == "" && got != tc.want:
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: got (%q, %v), want an error containing %q", tc.name, got, err, tc.wantErr)
		}
	}
}

// TestNTLMPerCallOwnsItsConnections runs concurrent calls against a server
// that always answers 401. Whatever the outcome of each call, every call must
// use connections of its own and close them: the server must see as many
// closed connections as it saw opened, and at least one per call. A shared,
// pooled transport would leave idle connections open and let calls reuse each
// other's.
func TestNTLMPerCallOwnsItsConnections(t *testing.T) {
	for _, encrypt := range []bool{false, true} {
		var mu sync.Mutex
		opened, closed := 0, 0
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("WWW-Authenticate", "Negotiate")
			w.WriteHeader(http.StatusUnauthorized)
		}))
		srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
			mu.Lock()
			defer mu.Unlock()
			switch s {
			case http.StateNew:
				opened++
			case http.StateClosed:
				closed++
			}
		}
		srv.Start()

		host, portText, _ := net.SplitHostPort(srv.Listener.Addr().String())
		port, _ := strconv.Atoi(portText)
		tp := &ntlmPerCall{user: `D\u`, password: "p", encrypt: encrypt}
		if err := tp.Transport(winrm.NewEndpoint(host, port, false, false, nil, nil, nil, 5*time.Second)); err != nil {
			t.Fatal(err)
		}

		const calls = 8
		var wg sync.WaitGroup
		for i := 0; i < calls; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := tp.Post(nil, soap.NewMessage()); err == nil {
					t.Error("expected an error from a server that always answers 401")
				}
			}()
		}
		wg.Wait()

		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			o, c := opened, closed
			mu.Unlock()
			if c == o && o >= calls {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("encrypt=%v: server saw %d connections opened and %d closed; want equal and at least %d", encrypt, o, c, calls)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		srv.Close()
	}
}
