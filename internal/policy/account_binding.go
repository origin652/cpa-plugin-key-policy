package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"path"
	"sort"
	"strings"
)

const (
	BindingStrategyWeightedRoundRobin = "weighted-round-robin"
	BindingStrategyRoundRobin         = "round-robin"
	BindingStrategyFillFirst          = "fill-first"
	CallerScopeMetadataKey            = "caller_scope"
)

// AccountBinding restricts a downstream key to auth IDs matched by Allow.
// A nil binding means unrestricted. A present binding with an empty Allow list
// intentionally matches nothing and therefore fails closed.
type AccountBinding struct {
	Allow    []string `yaml:"allow" json:"allow"`
	Strategy string   `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

func normalizeAccountBinding(binding *AccountBinding) error {
	if binding == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(binding.Allow))
	allow := make([]string, 0, len(binding.Allow))
	for _, raw := range binding.Allow {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			return errors.New("allow patterns cannot be empty")
		}
		if _, err := path.Match(pattern, "validation-probe"); err != nil {
			return err
		}
		if _, exists := seen[pattern]; exists {
			continue
		}
		seen[pattern] = struct{}{}
		allow = append(allow, pattern)
	}
	binding.Allow = allow
	switch strings.ToLower(strings.NewReplacer("_", "-", " ", "").Replace(strings.TrimSpace(binding.Strategy))) {
	case "", "weighted-round-robin", "weightedroundrobin", "wrr":
		binding.Strategy = BindingStrategyWeightedRoundRobin
	case "round-robin", "roundrobin", "rr":
		binding.Strategy = BindingStrategyRoundRobin
	case "fill-first", "fillfirst", "ff":
		binding.Strategy = BindingStrategyFillFirst
	default:
		return errors.New("strategy must be weighted-round-robin, round-robin, or fill-first")
	}
	return nil
}

func (binding *AccountBinding) Matches(authID string) bool {
	if binding == nil {
		return true
	}
	for _, pattern := range binding.Allow {
		matched, err := path.Match(pattern, authID)
		if err == nil && matched {
			return true
		}
	}
	return false
}

// CallerScopeForKey matches CLIProxyAPI session.CallerScope. It is
// domain-separated from HashKey and safe to persist instead of plaintext.
func CallerScopeForKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + key))
	return hex.EncodeToString(sum[:])
}

// HeaderCredentials returns all distinct credential values from supported
// headers. Keeping every value makes conflicting credentials explicit instead
// of silently selecting whichever header happens to win precedence.
func HeaderCredentials(headers http.Header) []string {
	if headers == nil {
		return nil
	}
	var values []string
	for _, name := range []string{"Authorization", "X-API-Key", "Api-Key", "X-Goog-Api-Key"} {
		for _, value := range headerValues(headers, name) {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if strings.EqualFold(name, "Authorization") {
				if token := bearerToken(value); token != "" {
					value = token
				}
			}
			values = append(values, value)
		}
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func ExtractHeaderAPIKey(headers http.Header) string {
	values := HeaderCredentials(headers)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func QueryCredentials(query map[string][]string) []string {
	if query == nil {
		return nil
	}
	var values []string
	for _, name := range []string{"api_key", "key", "auth_token"} {
		for _, value := range query[name] {
			if value = strings.TrimSpace(value); value != "" {
				values = append(values, value)
			}
		}
	}
	return values
}

func headerValues(headers http.Header, name string) []string {
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}
