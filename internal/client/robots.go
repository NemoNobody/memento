package client

// robots.txt handling (RFC 9309). robots is a crawler protocol: it governs
// automated browsers hammering sites, not a user-initiated single fetch of a
// document, so honoring it is opt-in (fetch_respect_robots). When enabled:
// robots.txt is fetched once per host and cached in-process for 24h; 404 and
// fetch/parse failures fail open (allow) — a robots outage must not break
// document access.

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/NemoNobody/memento/internal/config"
)

// robotsTTL bounds how long a fetched robots.txt is reused.
const robotsTTL = 24 * time.Hour

type robotsGroup struct {
	agents   []string
	disallow []string
	allow    []string
}

type robotsEntry struct {
	groups  []robotsGroup
	fetched time.Time
	bad     bool // fetch/parse failure: fail open
}

var robotsCache = struct {
	sync.Mutex
	m map[string]robotsEntry
}{m: map[string]robotsEntry{}}

// CheckRobots returns a non-nil error when robots.txt disallows the URL
// path for the configured user agent. It is a no-op unless the feature is
// enabled, and fails open on any robots outage.
func CheckRobots(ctx context.Context, cfg *config.Config, normalizedURL string) error {
	if !cfg.FetchRespectRobots {
		return nil
	}
	u, err := url.Parse(normalizedURL)
	if err != nil {
		return nil // normalization ran first; a parse failure here is not our problem
	}
	e := robotsFor(ctx, cfg, strings.ToLower(u.Scheme), strings.ToLower(u.Host))
	if e.bad {
		return nil
	}
	if robotsAllowed(e, uaToken(cfg.FetchUserAgent), u.Path) {
		return nil
	}
	return fmt.Errorf("fetch: robots.txt of %s disallows %q for this user agent", u.Host, u.Path)
}

func robotsFor(ctx context.Context, cfg *config.Config, scheme, host string) robotsEntry {
	key := scheme + "://" + host
	robotsCache.Lock()
	e, ok := robotsCache.m[key]
	robotsCache.Unlock()
	if ok && time.Since(e.fetched) < robotsTTL {
		return e
	}

	// The fetch runs outside the lock: one slow robots.txt (up to the fetch
	// timeout) must not serialize every other fetch in a long-lived server.
	_, body, _, _, err := fetchHTTP(ctx, cfg, key+"/robots.txt")
	e = robotsEntry{fetched: time.Now()}
	switch {
	case err != nil:
		e.bad = true // includes 404: no ruleset means no restrictions
	case len(strings.TrimSpace(string(body))) == 0:
		e.bad = true
	default:
		e.groups = parseRobots(string(body))
	}
	if ctx.Err() != nil {
		return e // the caller's cancellation says nothing about the host: do not cache it
	}
	robotsCache.Lock()
	sweepRobotsCacheLocked()
	robotsCache.m[key] = e
	robotsCache.Unlock()
	return e
}

// sweepRobotsCacheLocked drops entries older than the TTL: the cache is
// process-lifetime, and a long-running MCP server must not accumulate every
// host it ever touched.
func sweepRobotsCacheLocked() {
	for k, e := range robotsCache.m {
		if time.Since(e.fetched) >= robotsTTL {
			delete(robotsCache.m, k)
		}
	}
}

// parseRobots groups directives into (user-agents, rules) blocks per
// RFC 9309. A "user-agent" line after any rule starts a new group; a
// second "user-agent" line before any rule adds that agent to the CURRENT
// group (one rule set can serve several agents — dropping all but the last
// name left the earlier agents unrestricted while the file clearly bound them).
func parseRobots(body string) []robotsGroup {
	var groups []robotsGroup
	var rules int
	add := func(agent string) {
		groups = append(groups, robotsGroup{agents: []string{agent}})
		rules = 0
	}
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if v == "" {
				continue
			}
			if len(groups) == 0 || rules > 0 {
				add(strings.ToLower(v))
			} else {
				groups[len(groups)-1].agents = append(groups[len(groups)-1].agents, strings.ToLower(v))
			}
		case "disallow":
			if len(groups) > 0 {
				groups[len(groups)-1].disallow = append(groups[len(groups)-1].disallow, v)
				rules++
			}
		case "allow":
			if len(groups) > 0 {
				groups[len(groups)-1].allow = append(groups[len(groups)-1].allow, v)
				rules++
			}
		}
	}
	return groups
}

// robotsAllowed merges the rules of every matching group (exact-agent groups
// supersede wildcard groups), then applies the longest matching rule; on
// equal length Allow wins over Disallow (RFC 9309 ordering).
func robotsAllowed(e robotsEntry, agent, path string) bool {
	// RFC 9309 2.2.1: when several groups match the user-agent, their rules
	// are COMBINED into one (the union) — first-wins dropped appended
	// groups, last-wins drops the earlier ones; both silently violate the
	// file. Within specificity classes the exact-agent groups govern and
	// supersede the wildcard groups entirely.
	var exactAllow, exactDeny, wildAllow, wildDeny []string
	sawExact := false
	for i := range e.groups {
		g := &e.groups[i]
		exact, wildcard := false, false
		for _, a := range g.agents {
			if a == agent {
				exact = true
			}
			if a == "*" {
				wildcard = true
			}
		}
		if exact {
			sawExact = true
			exactAllow = append(exactAllow, g.allow...)
			exactDeny = append(exactDeny, g.disallow...)
		} else if wildcard {
			wildAllow = append(wildAllow, g.allow...)
			wildDeny = append(wildDeny, g.disallow...)
		}
	}
	allowRules, denyRules := wildAllow, wildDeny
	if sawExact {
		allowRules, denyRules = exactAllow, exactDeny
	}
	if allowRules == nil && denyRules == nil {
		return true
	}
	var allowLen, denyLen int
	for _, p := range allowRules {
		if p != "" && matchRobotPath(p, path) && len(p) > allowLen {
			allowLen = len(p)
		}
	}
	for _, p := range denyRules {
		if p == "" {
			continue // an empty Disallow means "allow everything"
		}
		if matchRobotPath(p, path) && len(p) > denyLen {
			denyLen = len(p)
		}
	}
	return allowLen >= denyLen
}

// matchRobotPath applies one RFC 9309 path rule: '*' matches any run of
// characters, and a rule ending in '$' must match the path end exactly.
// Before this, rules were plain prefixes: "/*.php$" never denied anything
// and "*" matched nothing — robots files that speak RFC 9309 were silently
// ignored, i.e. the crawler ignored a rule it appeared to honor.
func matchRobotPath(rule, path string) bool {
	endAnchor := strings.HasSuffix(rule, "$")
	pattern := strings.TrimSuffix(rule, "$")
	if !strings.Contains(pattern, "*") {
		if endAnchor {
			return path == pattern
		}
		return strings.HasPrefix(path, pattern)
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	rest := path[len(parts[0]):]
	last := parts[len(parts)-1]
	if endAnchor && last != "" {
		// The anchored suffix is verified FIRST and cut away before the
		// middle parts match: checking it after leftmost matching let a rule
		// like "/*.php$" miss "/x.php.php" (the leftmost ".php" does not end
		// the path), and left matching middle parts could overlap the suffix
		// region and accept paths the rule does not cover.
		if !strings.HasSuffix(rest, last) {
			return false
		}
		rest = rest[:len(rest)-len(last)]
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		i := strings.Index(rest, part)
		if i < 0 {
			return false
		}
		rest = rest[i+len(part):]
	}
	if endAnchor || last == "" {
		// A trailing '*' absorbs everything left; an anchored rule already
		// verified its suffix.
		return true
	}
	return strings.Contains(rest, last)
}

// uaToken extracts the RFC 9309 product token from a User-Agent string:
// the leading identifier before a "/" or space ("memento/fetch (x)" →
// "memento"). robots matches products, not full UA strings.
func uaToken(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	var b strings.Builder
	for _, r := range ua {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			break
		}
	}
	return b.String()
}
