package htmlutil

import (
	"bytes"
	"strings"

	"github.com/panphora/htmlclay/internal/config"
	"golang.org/x/net/html"
)

const (
	maxHelperNames = 8
	// Declarations after this prefix are ignored, so authors should put them
	// first in the head of a single-file application.
	helperScanLimit = 512 << 10
)

// ReadHelperNames returns the helper names a document declares, in document
// order, deduplicated, at most maxHelperNames. Invalid names are skipped rather
// than erroring: a bad meta tag is ignored the way an unknown meta tag is.
func ReadHelperNames(data []byte) []string {
	if len(data) > helperScanLimit {
		data = data[:helperScanLimit]
	}
	z := html.NewTokenizer(bytes.NewReader(data))
	seen := make(map[string]bool)
	names := make([]string, 0, maxHelperNames)
	templateDepth := 0
	rawElement := ""
	for {
		tokenType := z.Next()
		if tokenType == html.ErrorToken {
			return names
		}
		token := z.Token()
		if rawElement != "" {
			if tokenType == html.EndTagToken && token.Data == rawElement {
				rawElement = ""
			}
			continue
		}
		if templateDepth > 0 {
			switch tokenType {
			case html.StartTagToken, html.SelfClosingTagToken:
				if token.Data == "template" {
					templateDepth++
				}
			case html.EndTagToken:
				if token.Data == "template" {
					templateDepth--
				}
			}
			continue
		}
		switch tokenType {
		case html.TextToken:
			if strings.Trim(token.Data, " \t\n\r\f") != "" {
				return names
			}
		case html.EndTagToken:
			if token.Data == "head" || token.Data == "body" || token.Data == "html" || token.Data == "br" {
				return names
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			if token.Data == "template" {
				templateDepth = 1
				continue
			}
			if !isHeadElement(token.Data) {
				return names
			}
			if isRawHeadElement(token.Data) {
				rawElement = token.Data
				continue
			}
			if token.Data != "meta" {
				continue
			}
			var metaName, content string
			for _, attr := range token.Attr {
				switch attr.Key {
				case "name":
					metaName = attr.Val
				case "content":
					content = attr.Val
				}
			}
			if metaName != "htmlclay-helper" || !config.ValidHelperName(content) || seen[content] {
				continue
			}
			seen[content] = true
			names = append(names, content)
			if len(names) == maxHelperNames {
				return names
			}
		}
	}
}

func isRawHeadElement(name string) bool {
	switch name {
	case "noscript", "script", "title", "noframes", "style":
		return true
	default:
		return false
	}
}

func isHeadElement(name string) bool {
	switch name {
	case "html", "head", "base", "basefont", "bgsound", "link", "meta", "noscript", "script", "title", "noframes", "style":
		return true
	default:
		return false
	}
}
