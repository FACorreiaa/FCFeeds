package parse

import (
	"io"
	"time"
)

// Parse detects the format and returns a normalized feed. feedURL is used to
// resolve relative links and to derive keys for items without one; now is
// the fallback publication time for undated items.
func Parse(body []byte, feedURL string, now time.Time) (*Feed, error) {
	kind, err := Detect(body)
	if err != nil {
		return nil, err
	}
	switch kind {
	case KindAtom:
		return parseAtom(body, feedURL, now)
	case KindJSON:
		return parseJSONFeed(body, feedURL, now)
	default:
		return parseRSS(body, feedURL, now)
	}
}

// charsetReader accepts the encodings encoding/xml refuses by default. Anything
// declaring a Latin-1 family charset is passed through byte-for-byte, which is
// lossy for accents but never fails the parse; everything else is treated as
// UTF-8. A ticker would rather show a mangled accent than no headline.
func charsetReader(_ string, input io.Reader) (io.Reader, error) {
	return input, nil
}
