package controller

import (
	"net/url"
	"regexp"
)

// urlShaped matches a URI with a scheme. Store drivers wrap the request URL
// into errors (httpstore and repomanager both do `"%s %s: %w"`); those strings
// can carry userinfo or a query token that must not land on status or Events.
var urlShaped = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s]+`)

// userVisibleError is the string that may be written onto a condition message
// or Event. The caller must still log the unsanitized error.
func userVisibleError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeUserVisible(err.Error())
}

// sanitizeUserVisible strips URL userinfo and query/fragment from err text so
// a class URL like https://user:secret@host/path?token=abc cannot leak to
// anyone who can read the Artifact or list Events.
func sanitizeUserVisible(s string) string {
	return urlShaped.ReplaceAllStringFunc(s, redactURL)
}

func redactURL(raw string) string {
	core, trail := splitTrailingPunct(raw)
	u, err := url.Parse(core)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String() + trail
}

func splitTrailingPunct(s string) (core, trail string) {
	i := len(s)
	for i > 0 {
		switch s[i-1] {
		case '.', ',', ';', ':', ')', ']', '\'', '"':
			i--
		default:
			return s[:i], s[i:]
		}
	}
	return s, ""
}
