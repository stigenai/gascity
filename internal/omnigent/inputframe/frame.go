// Package inputframe defines the terminal-safe message boundary used between
// Gas City's interactive runtimes and an Omnigent controller attachment.
package inputframe

import (
	"encoding/base64"
	"errors"
	"strings"
)

const (
	// ControllerProvider is the composed provider exposed by the Gas City
	// Omnigent integration. Only this provider receives framed terminal input.
	ControllerProvider = "omnigent-city"

	prefix = "gc-omnigent-input-v1:"
)

// Encode turns one semantic message into one printable terminal line. Newlines
// in the message are data, not message delimiters.
func Encode(message string) string {
	return prefix + base64.RawURLEncoding.EncodeToString([]byte(message))
}

// EncodeForProvider applies controller framing only to the composed Omnigent
// provider. Other terminal providers retain their existing input contract.
func EncodeForProvider(provider, message string) string {
	if strings.TrimSpace(provider) != ControllerProvider {
		return message
	}
	return Encode(message)
}

// Decode verifies and decodes one controller input line.
func Decode(line string) (string, error) {
	if !strings.HasPrefix(line, prefix) {
		return "", errors.New("missing Omnigent controller input frame")
	}
	payload := strings.TrimPrefix(line, prefix)
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("invalid Omnigent controller input frame")
	}
	return string(decoded), nil
}

// MaxEncodedLen returns the largest valid framed-line length for a decoded
// message of maxMessageBytes bytes.
func MaxEncodedLen(maxMessageBytes int) int {
	return len(prefix) + base64.RawURLEncoding.EncodedLen(maxMessageBytes)
}
