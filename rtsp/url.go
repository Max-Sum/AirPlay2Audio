package rtsp

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// PathSplitQuery splits a path from a query.
func PathSplitQuery(pathAndQuery string) (string, string) {
	i := strings.Index(pathAndQuery, "?")
	if i >= 0 {
		return pathAndQuery[:i], pathAndQuery[i:]
	}

	return pathAndQuery, ""
}

// URL is a RTSP URL.
// This is basically an HTTP URL with some additional functions to handle
// control attributes.
type URL url.URL

// ParseURL parses a RTSP URL.
func ParseURL(s string) (*URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}

	if u.Opaque != "" {
		return nil, fmt.Errorf("URLs with opaque data are not supported")
	}

	if u.Fragment != "" {
		return nil, fmt.Errorf("URLs with fragments are not supported")
	}

	return (*URL)(u), nil
}

// splitBareURL splits an rtsp:// URL whose host net/url cannot parse (an
// IPv6 address without brackets) into host, path and query.
func splitBareURL(s string) *URL {
	const scheme = "rtsp://"
	if len(s) <= len(scheme) || !strings.EqualFold(s[:len(scheme)], scheme) {
		return nil
	}
	rest := s[len(scheme):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return nil
	}
	host, pathAndQuery := rest[:slash], rest[slash:]
	if net.ParseIP(strings.SplitN(host, "%", 2)[0]) == nil {
		return nil
	}
	path, query := PathSplitQuery(pathAndQuery)
	return &URL{Scheme: "rtsp", Host: "[" + host + "]", Path: path, RawQuery: strings.TrimPrefix(query, "?")}
}

// String implements fmt.Stringer.
func (u *URL) String() string {
	return (*url.URL)(u).String()
}

// Clone clones a URL.
func (u *URL) Clone() *URL {
	return (*URL)(&url.URL{
		Scheme:     u.Scheme,
		User:       u.User,
		Host:       u.Host,
		Path:       u.Path,
		RawPath:    u.RawPath,
		ForceQuery: u.ForceQuery,
		RawQuery:   u.RawQuery,
	})
}

// CloneWithoutCredentials clones a URL without its credentials.
func (u *URL) CloneWithoutCredentials() *URL {
	return (*URL)(&url.URL{
		Scheme:     u.Scheme,
		Host:       u.Host,
		Path:       u.Path,
		RawPath:    u.RawPath,
		ForceQuery: u.ForceQuery,
		RawQuery:   u.RawQuery,
	})
}

// RTSPPath returns the path of a RTSP URL.
func (u *URL) RTSPPath() (string, bool) {
	pathAndQuery, ok := u.RTSPPathAndQuery()
	if !ok {
		return "", false
	}

	path, _ := PathSplitQuery(pathAndQuery)
	return path, true
}

// RTSPPathAndQuery returns the path and query of a RTSP URL.
func (u *URL) RTSPPathAndQuery() (string, bool) {
	var pathAndQuery string
	if u.RawPath != "" {
		pathAndQuery = u.RawPath
	} else {
		pathAndQuery = u.Path
	}
	if u.RawQuery != "" {
		pathAndQuery += "?" + u.RawQuery
	}

	// remove leading slash
	if len(pathAndQuery) == 0 || pathAndQuery[0] != '/' {
		return "", false
	}
	pathAndQuery = pathAndQuery[1:]

	return pathAndQuery, true
}

