package parse

import (
	"bytes"
	"errors"
	"regexp"
)

var (
	errUnknownFormat = errors.New("parse: not an RSS, Atom or JSON Feed document")

	reAtomRoot = regexp.MustCompile(`(?is)<feed[\s>][^>]*xmlns\s*=\s*["']http://www\.w3\.org/2005/Atom["']`)
	reRSSRoot  = regexp.MustCompile(`(?is)<(rss|rdf:RDF)[\s>]`)
)

// Detect sniffs the document format without fully parsing it.
func Detect(body []byte) (Kind, error) {
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	trimmed := bytes.TrimLeft(head, " \t\r\n\xef\xbb\xbf")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if bytes.Contains(head, []byte("jsonfeed.org/version")) {
			return KindJSON, nil
		}
		return "", errUnknownFormat
	}
	if reAtomRoot.Match(head) {
		return KindAtom, nil
	}
	if reRSSRoot.Match(head) {
		return KindRSS, nil
	}
	return "", errUnknownFormat
}
