package domain

import (
	"regexp"
	"strings"
)

// Reference is one `${{ … }}` reference found in a variable value.
type Reference struct {
	// Raw is the full matched text, including the delimiters.
	Raw string
	// Service is the target service name; empty means the same service
	// (or the shared scope when Shared is set).
	Service string
	Shared  bool
	Name    string
}

var referencePattern = regexp.MustCompile(`\$\{\{\s*([^}]*?)\s*\}\}`)

var quotedReferencePattern = regexp.MustCompile(`^"([^"]+)"\.(.+)$`)

// ParseReferences returns every `${{ … }}` in value: `VAR`, `svc.VAR`,
// `"svc name".VAR`, and `shared.VAR`.
func ParseReferences(value string) []Reference {
	var refs []Reference
	for _, match := range referencePattern.FindAllStringSubmatch(value, -1) {
		raw := match[0]
		inner := strings.TrimSpace(match[1])

		if quoted := quotedReferencePattern.FindStringSubmatch(inner); quoted != nil {
			refs = append(refs, Reference{Raw: raw, Service: quoted[1], Name: quoted[2]})
			continue
		}

		dot := strings.LastIndex(inner, ".")
		if dot == -1 {
			refs = append(refs, Reference{Raw: raw, Name: inner})
			continue
		}

		service := inner[:dot]
		name := inner[dot+1:]
		if service == "shared" {
			refs = append(refs, Reference{Raw: raw, Shared: true, Name: name})
		} else {
			refs = append(refs, Reference{Raw: raw, Service: service, Name: name})
		}
	}
	return refs
}

// IsLiteral reports whether value contains no references.
func IsLiteral(value string) bool {
	return len(ParseReferences(value)) == 0
}
