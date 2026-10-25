package clayruntime

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// LocalPrefix is the path prefix every projected runtime URL carries.
const LocalPrefix = "/_/clayjs/"

var (
	controlPattern = regexp.MustCompile(`[\x00-\x1f\x7f]`)
	edgeSpace      = regexp.MustCompile(`^[\t\n\f\r ]|[\t\n\f\r ]$`)
	rollingPattern = regexp.MustCompile(`(?s)^https://clayjs\.com/v([0-9]+)/(.*)$`)
	exactPattern   = regexp.MustCompile(`(?s)^https://clayjs\.com/([0-9]+)\.([0-9]+)\.([0-9]+)/(.*)$`)
	localPattern   = regexp.MustCompile(`(?s)^/_/clayjs/([0-9]+)\.([0-9]+)\.([0-9]+)/(.*)$`)
	suspectPattern = regexp.MustCompile(`(?i)%2e|%2f|%5c`)
	versionPattern = regexp.MustCompile(`^([0-9]+)\.[0-9]+\.[0-9]+$`)
)

// Options names the releases a URL resolves against, the rolling release, and the
// document a same-origin local URL has to share.
type Options struct {
	Releases        map[string]Release
	SelectedVersion string
	DocumentURL     string
}

// CanonOptions adds the descriptor a served page carries back to a local URL.
type CanonOptions struct {
	Descriptor  *Descriptor
	Releases    map[string]Release
	DocumentURL string
}

// Parsed is what parseRuntimeURL reads out of a runtime URL.
type Parsed struct {
	Kind    string
	Major   string
	Version string
	RawPath string
	Path    string
	Search  string
	Hash    string
}

// Resolution is how one URL maps to the bundled release.
type Resolution struct {
	Kind         string `json:"kind"`
	Version      string `json:"version"`
	Path         string `json:"path"`
	Search       string `json:"search"`
	Hash         string `json:"hash"`
	Available    bool   `json:"available"`
	CanonicalURL string `json:"canonicalURL"`
	LocalURL     string `json:"-"`
}

// MarshalJSON writes localURL as null when the release it names is not bundled.
func (resolution Resolution) MarshalJSON() ([]byte, error) {
	type wire struct {
		Kind         string  `json:"kind"`
		Version      string  `json:"version"`
		Path         string  `json:"path"`
		Search       string  `json:"search"`
		Hash         string  `json:"hash"`
		Available    bool    `json:"available"`
		CanonicalURL string  `json:"canonicalURL"`
		LocalURL     *string `json:"localURL"`
	}
	var local *string
	if resolution.Available {
		value := resolution.LocalURL
		local = &value
	}
	return json.Marshal(wire{
		Kind:         resolution.Kind,
		Version:      resolution.Version,
		Path:         resolution.Path,
		Search:       resolution.Search,
		Hash:         resolution.Hash,
		Available:    resolution.Available,
		CanonicalURL: resolution.CanonicalURL,
		LocalURL:     local,
	})
}

// Descriptor is a served page's `clay-runtime-map` JSON.
type Descriptor struct {
	Entries []map[string]any
	Aliases map[string]any
}

// ParseDescriptor reads a descriptor's JSON, returning nil when it is not an object.
func ParseDescriptor(data []byte) *Descriptor {
	var value any
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&value); err != nil {
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	descriptor := &Descriptor{}
	if entries, ok := object["entries"].([]any); ok {
		for _, entry := range entries {
			if fields, ok := entry.(map[string]any); ok {
				descriptor.Entries = append(descriptor.Entries, fields)
			}
		}
	}
	switch aliases := object["aliases"].(type) {
	case map[string]any:
		descriptor.Aliases = aliases
	case []any:
		indexed := make(map[string]any, len(aliases))
		for index, entry := range aliases {
			indexed[strconv.Itoa(index)] = entry
		}
		descriptor.Aliases = indexed
	}
	return descriptor
}

func majorOf(version string) string {
	match := versionPattern.FindStringSubmatch(version)
	if match == nil {
		return ""
	}
	return match[1]
}

func originOf(documentURL string) string {
	if documentURL == "" {
		return ""
	}
	parsed, err := url.Parse(documentURL)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	switch {
	case port == "":
	case scheme == "http" && port == "80":
	case scheme == "https" && port == "443":
	default:
		host += ":" + port
	}
	return scheme + "://" + host
}

func splitURL(raw string) (head, search, hash string) {
	hashAt := strings.Index(raw, "#")
	hash = ""
	withoutHash := raw
	if hashAt >= 0 {
		hash = raw[hashAt:]
		withoutHash = raw[:hashAt]
	}
	searchAt := strings.Index(withoutHash, "?")
	search = ""
	head = withoutHash
	if searchAt >= 0 {
		search = withoutHash[searchAt:]
		head = withoutHash[:searchAt]
	}
	return head, search, hash
}

func unhex(char byte) (byte, bool) {
	switch {
	case char >= '0' && char <= '9':
		return char - '0', true
	case char >= 'a' && char <= 'f':
		return char - 'a' + 10, true
	case char >= 'A' && char <= 'F':
		return char - 'A' + 10, true
	default:
		return 0, false
	}
}

func decodeURIComponent(raw string) (string, bool) {
	decoded := make([]byte, 0, len(raw))
	for index := 0; index < len(raw); index++ {
		if raw[index] != '%' {
			decoded = append(decoded, raw[index])
			continue
		}
		if index+2 >= len(raw) {
			return "", false
		}
		high, ok := unhex(raw[index+1])
		if !ok {
			return "", false
		}
		low, ok := unhex(raw[index+2])
		if !ok {
			return "", false
		}
		decoded = append(decoded, high<<4|low)
		index += 2
	}
	if !utf8.Valid(decoded) {
		return "", false
	}
	return string(decoded), true
}

func decodePath(rawPath string) (string, bool) {
	if rawPath == "" {
		return "", false
	}
	if strings.Contains(rawPath, `\`) {
		return "", false
	}
	if suspectPattern.MatchString(rawPath) {
		return "", false
	}
	segments := strings.Split(rawPath, "/")
	decoded := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
		value, ok := decodeURIComponent(segment)
		if !ok {
			return "", false
		}
		if strings.Contains(value, "/") || strings.Contains(value, `\`) {
			return "", false
		}
		if value == "." || value == ".." {
			return "", false
		}
		decoded = append(decoded, value)
	}
	return strings.Join(decoded, "/"), true
}

// ParseRuntimeURL reads a ClayJS runtime URL, or nil when the URL is not one.
func ParseRuntimeURL(raw string, opts Options) *Parsed {
	if raw == "" {
		return nil
	}
	if edgeSpace.MatchString(raw) || controlPattern.MatchString(raw) {
		return nil
	}
	head, search, hash := splitURL(raw)

	kind := ""
	version := ""
	major := ""
	rawPath := ""
	if match := rollingPattern.FindStringSubmatch(head); match != nil {
		kind = "rolling"
		major = match[1]
		rawPath = match[2]
	}
	if kind == "" {
		if match := exactPattern.FindStringSubmatch(head); match != nil {
			kind = "exact"
			version = match[1] + "." + match[2] + "." + match[3]
			major = match[1]
			rawPath = match[4]
		}
	}
	if kind == "" {
		if match := localPattern.FindStringSubmatch(head); match != nil {
			kind = "local"
			version = match[1] + "." + match[2] + "." + match[3]
			major = match[1]
			rawPath = match[4]
		}
	}
	if kind == "" {
		origin := originOf(opts.DocumentURL)
		if origin != "" && strings.HasPrefix(head, origin+"/") {
			if match := localPattern.FindStringSubmatch(head[len(origin):]); match != nil {
				kind = "local"
				version = match[1] + "." + match[2] + "." + match[3]
				major = match[1]
				rawPath = match[4]
			}
		}
	}
	if kind == "" {
		return nil
	}

	path, ok := decodePath(rawPath)
	if !ok {
		return nil
	}
	return &Parsed{Kind: kind, Major: major, Version: version, RawPath: rawPath, Path: path, Search: search, Hash: hash}
}

func filesOf(releases map[string]Release, version string) map[string]File {
	if releases == nil {
		return nil
	}
	release, ok := releases[version]
	if !ok || release.Files == nil {
		return nil
	}
	return release.Files
}

// ResolveRuntimeURL maps a ClayJS runtime URL onto the bundled release, or nil.
func ResolveRuntimeURL(raw string, opts Options) *Resolution {
	if filesOf(opts.Releases, opts.SelectedVersion) != nil {
		aliases := opts.Releases[opts.SelectedVersion].Aliases
		if target, ok := aliases[raw]; ok {
			return &Resolution{
				Kind:         "alias",
				Version:      opts.SelectedVersion,
				Path:         target,
				Available:    true,
				CanonicalURL: raw,
				LocalURL:     LocalPrefix + opts.SelectedVersion + "/" + target,
			}
		}
	}

	parsed := ParseRuntimeURL(raw, opts)
	if parsed == nil {
		return nil
	}
	if parsed.Kind == "local" {
		return nil
	}

	version := ""
	if parsed.Kind == "rolling" {
		selectedMajor := majorOf(opts.SelectedVersion)
		if selectedMajor == "" || selectedMajor != parsed.Major {
			return nil
		}
		version = opts.SelectedVersion
	} else {
		version = parsed.Version
		if filesOf(opts.Releases, version) == nil {
			return &Resolution{
				Kind:         "exact",
				Version:      version,
				Path:         parsed.Path,
				Search:       parsed.Search,
				Hash:         parsed.Hash,
				Available:    false,
				CanonicalURL: raw,
			}
		}
	}

	files := filesOf(opts.Releases, version)
	if files == nil {
		return nil
	}
	if _, ok := files[parsed.Path]; !ok {
		return nil
	}
	return &Resolution{
		Kind:         parsed.Kind,
		Version:      version,
		Path:         parsed.Path,
		Search:       parsed.Search,
		Hash:         parsed.Hash,
		Available:    true,
		CanonicalURL: raw,
		LocalURL:     LocalPrefix + version + "/" + parsed.RawPath + parsed.Search + parsed.Hash,
	}
}

func stringField(fields map[string]any, name string) (string, bool) {
	value, ok := fields[name].(string)
	return value, ok
}

func aliasTargetOf(releases map[string]Release, version string) map[string]string {
	if releases == nil {
		return nil
	}
	release, ok := releases[version]
	if !ok {
		return nil
	}
	return release.Aliases
}

func originalMatchesRelease(path, version string, releases map[string]Release) bool {
	if releases == nil {
		return true
	}
	files := filesOf(releases, version)
	if files == nil {
		return false
	}
	_, ok := files[path]
	return ok
}

func declaredAliasTarget(descriptor *Descriptor, key string) (string, bool) {
	if descriptor == nil || descriptor.Aliases == nil {
		return "", false
	}
	target, ok := descriptor.Aliases[key].(string)
	return target, ok
}

// CanonicalizeRuntimeURL turns a local runtime URL back into the URL it was projected from.
func CanonicalizeRuntimeURL(value string, opts CanonOptions) string {
	local := ParseRuntimeURL(value, Options{DocumentURL: opts.DocumentURL})
	if local == nil || local.Kind != "local" {
		return value
	}

	var entries []map[string]any
	if opts.Descriptor != nil {
		entries = opts.Descriptor.Entries
	}
	localMajor := majorOf(local.Version)

	for _, entry := range entries {
		localValue, hasLocal := stringField(entry, "local")
		original, hasOriginal := stringField(entry, "original")
		if !hasLocal || localValue != value || !hasOriginal {
			continue
		}
		if aliases := aliasTargetOf(opts.Releases, local.Version); aliases != nil &&
			local.Search == "" && local.Hash == "" {
			if target, ok := aliases[original]; ok && target == local.Path {
				return original
			}
		}
		if opts.Releases == nil && local.Search == "" && local.Hash == "" && strings.HasPrefix(original, "https://") {
			if target, ok := declaredAliasTarget(opts.Descriptor, original); ok && target == local.Path {
				return original
			}
		}
		parsed := ParseRuntimeURL(original, Options{})
		if parsed == nil {
			continue
		}
		if parsed.Kind != "rolling" && parsed.Kind != "exact" {
			continue
		}
		if parsed.RawPath != local.RawPath || parsed.Search != local.Search || parsed.Hash != local.Hash {
			continue
		}
		if parsed.Kind == "rolling" {
			if parsed.Major != localMajor {
				continue
			}
		} else if parsed.Version != local.Version {
			continue
		}
		if !originalMatchesRelease(local.Path, local.Version, opts.Releases) {
			continue
		}
		if parsed.Kind == "exact" {
			return original
		}
		return "https://clayjs.com/v" + parsed.Major + "/" + local.RawPath + local.Search + local.Hash
	}

	for _, entry := range entries {
		original, ok := stringField(entry, "original")
		if !ok {
			continue
		}
		parsed := ParseRuntimeURL(original, Options{})
		if parsed == nil {
			continue
		}
		if parsed.Kind != "rolling" && parsed.Kind != "exact" {
			continue
		}
		if parsed.RawPath != local.RawPath {
			continue
		}
		if parsed.Kind == "rolling" {
			if parsed.Major != localMajor {
				continue
			}
			return "https://clayjs.com/v" + parsed.Major + "/" + local.RawPath + local.Search + local.Hash
		}
		if parsed.Version != local.Version {
			continue
		}
		return "https://clayjs.com/" + parsed.Version + "/" + local.RawPath + local.Search + local.Hash
	}

	return "https://clayjs.com/" + local.Version + "/" + local.RawPath + local.Search + local.Hash
}
