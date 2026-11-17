package domain

import (
	"net/url"
	"path"
	"strings"
)

// CanonicalPrefix returns a URL's lowercase host name (no port or
// userinfo), a file URL's directory, or a slash-normalized source
// directory. A bare filename has no prefix, and so has a URL that does not
// parse or names no host: no prior beats a wrong one.
func CanonicalPrefix(source string) string {
	if strings.Contains(source, "://") {
		u, err := url.Parse(source)
		switch {
		case err != nil:
			return ""
		case u.Host != "":
			return strings.ToLower(u.Hostname())
		case u.Scheme != "file":
			return ""
		}
		source = u.Path
	}
	dir := path.Dir(strings.ReplaceAll(source, `\`, "/"))
	if dir == "." {
		return ""
	}
	return dir
}

func prefixDomain(source string, corpus []DocProfile) (string, bool) {
	prefix := CanonicalPrefix(source)
	if prefix == "" {
		return "", false
	}
	var domain string
	var count int
	for _, doc := range corpus {
		if CanonicalPrefix(doc.Source) != prefix {
			continue
		}
		if count > 0 && doc.Domain != domain {
			return "", false
		}
		domain = doc.Domain
		count++
	}
	return domain, count >= 2
}
