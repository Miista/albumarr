package notify

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Server is how to reach and authenticate to an ntfy server.
type Server struct {
	URL      string
	Auth     string
	Username string
	Password string
	Token    string
}

// Message is one notification, as ntfy takes it.
type Message struct {
	Topic    string
	Title    string
	Body     string
	Click    string // opened when the notification is tapped
	Attach   string // an image shown with it
	Priority int    // 1 to 5; 0 means ntfy's default
	Tags     []string
}

// Sender publishes messages. It is an interface so tests need no server.
type Sender interface {
	Send(ctx context.Context, srv Server, m Message) error
}

const (
	sendTimeout  = 10 * time.Second
	maxTitleLen  = 250
	maxBodyLen   = 3500 // ntfy's own limit is 4096 bytes
	maxErrorBody = 512
)

// Client publishes to ntfy over its JSON API: one POST to the server's root, so the
// topic and everything else travel in the body, never in a URL that could be logged.
type Client struct{ http *http.Client }

// NewClient returns a Client. It does not follow redirects: a server that sends us
// elsewhere is not one to hand credentials on to.
func NewClient() *Client {
	return &Client{http: &http.Client{
		Timeout:       sendTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type publishBody struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Click    string   `json:"click,omitempty"`
	Attach   string   `json:"attach,omitempty"`
}

// Send publishes m. Errors never contain the server's address, the topic, or any
// credential, so they are safe to log and to show to an admin.
func (c *Client) Send(ctx context.Context, srv Server, m Message) error {
	if m.Topic == "" {
		return errors.New("notify: no topic")
	}
	body, err := json.Marshal(publishBody{
		Topic: m.Topic, Title: cut(m.Title, maxTitleLen), Message: cut(m.Body, maxBodyLen),
		Priority: m.Priority, Tags: m.Tags, Click: m.Click, Attach: m.Attach,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(srv.URL, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return errors.New("the ntfy address is not usable")
	}
	req.Header.Set("Content-Type", "application/json")
	switch srv.Auth {
	case AuthBasic:
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(srv.Username+":"+srv.Password)))
	case AuthToken:
		req.Header.Set("Authorization", "Bearer "+srv.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("could not reach the ntfy server: %s", why(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	// ntfy explains itself in JSON ({"code":..,"error":"..."}); quote only that.
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&e)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("the ntfy server refused the credentials or the topic (%d)", resp.StatusCode)
	case e.Error != "":
		return fmt.Errorf("the ntfy server answered %d: %s", resp.StatusCode, cut(e.Error, 200))
	}
	return fmt.Errorf("the ntfy server answered %d", resp.StatusCode)
}

// why names what went wrong on the network in fixed words. The error Go gives
// ("dial tcp 192.0.2.5:80: ...") carries the server's address, which a person who
// only asked for a test to their own phone has no business learning.
func why(err error) string {
	var dns *net.DNSError
	var cert x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &dns):
		return "the server's name could not be found"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused"
	case errors.Is(err, context.DeadlineExceeded):
		return "it took too long to answer"
	case errors.As(err, &cert), errors.As(err, &hostErr), errors.As(err, &invalid):
		return "its certificate was not accepted"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "it took too long to answer"
	}
	return "a network error"
}

// cut trims to at most n bytes without splitting a character.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}
