package review

import (
	"bytes"
	"encoding/base64"
	"math"
	"regexp"
	"strings"
)

const secretRedactionMarker = "[redacted: secret-like content]"

// secretPattern keeps the content policy data-driven. valueSubmatch identifies
// the capture whose entropy is measured; zero means the complete match.
type secretPattern struct {
	name          string
	expression    *regexp.Regexp
	valueSubmatch int
	minEntropy    float64
	validate      func(string) bool
}

var secretContentPatterns = []secretPattern{
	{name: "aws-access-key", expression: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{name: "generic-secret-assignment", expression: regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|secret)\b[[:space:]]*[:=][[:space:]]*["']?([A-Za-z0-9_./+=-]{16,})`), valueSubmatch: 1, minEntropy: 3.5},
	{name: "private-key-header", expression: regexp.MustCompile(`-----BEGIN(?:[[:space:]]+[A-Z0-9]+)*[[:space:]]+PRIVATE[[:space:]]+KEY-----`)},
	{name: "jwt", expression: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b`)},
	{name: "github-token", expression: regexp.MustCompile(`\bgh[ops]_[A-Za-z0-9]{30,}\b`)},
	{name: "slack-token", expression: regexp.MustCompile(`\bxox[A-Za-z]-[A-Za-z0-9-]{10,}\b`)},
	{name: "connection-string-uri-password", expression: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^:/[:space:]'\"]+:([^@/[:space:]'\"]+)@`)},
	{name: "connection-string-password", expression: regexp.MustCompile(`(?i)\b(?:server|host|data[ _]source|database)[[:space:]]*=[^\r\n]{0,256};[^\r\n]{0,256}\b(?:password|pwd)[[:space:]]*=[[:space:]]*([^;\r\n]+)`)},
	{name: "connection-string-password-first", expression: regexp.MustCompile(`(?i)\b(?:password|pwd)[[:space:]]*=[[:space:]]*[^;\r\n]+;[^\r\n]{0,256}\b(?:server|host|data[ _]source|database)[[:space:]]*=`)},
	{name: "hex-assignment", expression: regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_.-]*[[:space:]]*[:=][[:space:]]*["']?([A-Fa-f0-9]{33,})`), valueSubmatch: 1, minEntropy: 3.5},
	{name: "base64-assignment", expression: regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_.-]*[[:space:]]*[:=][[:space:]]*["']?([A-Za-z0-9+/_=-]{33,})`), valueSubmatch: 1, minEntropy: 4.3, validate: isBase64Value},
}

var allHexValue = regexp.MustCompile(`^[A-Fa-f0-9]+$`)

// redactSecretLikeContent redacts matching diff hunks after every changed file
// has been assembled. Path/status entries are rendered separately and remain
// available to the reviewer.
func redactSecretLikeContent(files []changedFile) {
	for i := range files {
		if files[i].Redacted || len(files[i].diff) == 0 {
			continue
		}
		files[i].diff = redactSecretLikeHunks(files[i].diff)
	}
}

func redactSecretLikeHunks(diff []byte) []byte {
	if !containsSecretLikeContent(diff) {
		return diff
	}

	starts := diffHunkStarts(diff)
	if len(starts) == 0 {
		return []byte(secretRedactionMarker + "\n")
	}

	var out bytes.Buffer
	preamble := diff[:starts[0]]
	if containsSecretLikeContent(preamble) {
		out.WriteString(secretRedactionMarker + "\n")
	} else {
		out.Write(preamble)
	}
	for i, start := range starts {
		end := len(diff)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		hunk := diff[start:end]
		if containsSecretLikeContent(hunk) {
			out.WriteString(secretRedactionMarker + "\n")
		} else {
			out.Write(hunk)
		}
	}
	return out.Bytes()
}

func diffHunkStarts(diff []byte) []int {
	var starts []int
	for offset := 0; offset < len(diff); {
		if bytes.HasPrefix(diff[offset:], []byte("@@ ")) {
			starts = append(starts, offset)
		}
		next := bytes.IndexByte(diff[offset:], '\n')
		if next < 0 {
			break
		}
		offset += next + 1
	}
	return starts
}

func containsSecretLikeContent(content []byte) bool {
	for _, pattern := range secretContentPatterns {
		if secretPatternMatches(pattern, content) {
			return true
		}
	}
	return false
}

func secretPatternMatches(pattern secretPattern, content []byte) bool {
	for _, match := range pattern.expression.FindAllSubmatch(content, -1) {
		candidate := string(match[0])
		if pattern.valueSubmatch > 0 {
			if pattern.valueSubmatch >= len(match) {
				continue
			}
			candidate = strings.TrimSpace(string(match[pattern.valueSubmatch]))
		}
		if pattern.minEntropy > 0 && shannonEntropy(candidate) < pattern.minEntropy {
			continue
		}
		if pattern.validate != nil && !pattern.validate(candidate) {
			continue
		}
		return true
	}
	return false
}

func shannonEntropy(value string) float64 {
	if value == "" {
		return 0
	}
	counts := make(map[byte]int)
	for i := 0; i < len(value); i++ {
		counts[value[i]]++
	}
	length := float64(len(value))
	var entropy float64
	for _, count := range counts {
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy
}

func isBase64Value(value string) bool {
	// Hex has its own lower-maximum-entropy rule and should be attributed there.
	if allHexValue.MatchString(value) {
		return false
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if _, err := encoding.DecodeString(value); err == nil {
			return true
		}
	}
	return false
}
