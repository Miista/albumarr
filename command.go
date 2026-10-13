package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"
)

// version is set when the binary is built (-ldflags "-X main.version=1.2.3"); a plain
// `go build` is "dev".
var version = "dev"

// command handles the few words the binary understands besides starting the server.
// The image has no shell and no curl, so its HEALTHCHECK is the binary itself:
//
//	albumreq healthcheck   exit 0 if the running server answers /healthz, else 1
//	albumreq version       print the version
//
// handled is false when there is nothing to do here and the server should start; an
// unknown word is handled with exit code 2, rather than starting the server on a typo.
func command(args []string, env func(string) string, out, errOut io.Writer) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "healthcheck":
		addr := env("ALBUMREQ_ADDR")
		if addr == "" {
			addr = defaultAddr
		}
		if err := healthcheck(healthURL(addr), 3*time.Second); err != nil {
			fmt.Fprintln(errOut, "unhealthy:", err)
			return true, 1
		}
		return true, 0
	case "version", "--version", "-version":
		fmt.Fprintln(out, "albumreq", version)
		return true, 0
	}
	fmt.Fprintln(errOut, "unknown command", fmt.Sprintf("%q", args[0])+": albumreq takes no arguments except healthcheck and version; it is configured by environment variables")
	return true, 2
}

// healthURL is where the server at a listen address answers /healthz, as seen from
// the same machine: a listen address with no host (":8768") or the wildcard is reached
// on the loopback.
func healthURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/healthz"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

// healthcheck asks url and wants a plain 200. A redirect is not followed: /healthz is
// answered directly, so a redirect means something else is on the port.
func healthcheck(url string, timeout time.Duration) error {
	c := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(url)
	if err != nil {
		return fmt.Errorf("no answer from %s", strings.TrimSuffix(url, "/healthz"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/healthz answered %s", resp.Status)
	}
	return nil
}

// permissionHint is what to say when the data directory cannot be written: in a
// container the usual cause is a directory owned by someone other than the user the
// image runs as. It adds nothing for any other failure.
func permissionHint(err error) string {
	if !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	return ": the directory must be writable by the user albumreq runs as (65532 unless you set --user or `user:`). Make the directory (a bind mount's source) owned by that user, or start the container with --user set to the directory's owner"
}
