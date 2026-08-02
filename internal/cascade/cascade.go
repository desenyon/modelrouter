// Package cascade decides whether an upstream failure should escalate tiers.
package cascade

import (
	"net"
	"strings"
)

// Retryable reports whether a transport/status failure should trigger one escalate.
func Retryable(status int, err error) bool {
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") {
			return true
		}
		var ne net.Error
		if ok := errorAsNet(err, &ne); ok && ne.Timeout() {
			return true
		}
		return true // connection refused / reset → retry other candidate
	}
	switch status {
	case 429, 502, 503, 504:
		return true
	default:
		return false
	}
}

func errorAsNet(err error, dest *net.Error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok {
		*dest = ne
		return true
	}
	return false
}

// Label formats a cascade header value.
func Label(fromTier, toTier, fromModel, toModel string) string {
	if fromTier != "" && toTier != "" && fromTier != toTier {
		return fromTier + "→" + toTier
	}
	return fromModel + "→" + toModel
}
