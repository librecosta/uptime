package config

import (
	"fmt"
	"regexp"
	"strings"
)

type envLookup func(string) (string, bool)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// interpolate expands only ${NAME} in the original input. Replacement values are
// opaque so secrets containing dollar signs cannot trigger recursive expansion.
func interpolate(value string, lookup envLookup) (string, error) {
	var out strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			out.WriteString(value)
			return out.String(), nil
		}
		out.WriteString(value[:start])
		value = value[start+2:]
		end := strings.IndexByte(value, '}')
		if end < 0 || !envName.MatchString(value[:end]) {
			return "", fmt.Errorf("invalid environment expression; expected ${VAR_NAME}")
		}
		name := value[:end]
		replacement, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		out.WriteString(replacement) // Never scan the replacement for expressions.
		value = value[end+1:]
	}
}
