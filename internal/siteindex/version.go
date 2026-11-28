// Package siteindex builds the merged mod.json that is published on GitHub Pages.
//
// It only uses the standard library, so it can be built and tested on its own.
package siteindex

import (
	"regexp"
	"strconv"
	"strings"
)

// Unknown is the version given to anything whose version can't be told. It always sorts last.
const Unknown = "unknown"

// Versions are kept as strings (not numbers): "1.0.2" isn't a number, and the consumer only
// logs the value, so all that matters is that equal versions get the same text and that the
// order is right.

var (
	// "v1.0", "version 2", "Final 1.1", "Reset Beta 1.0" -> the number after the label.
	versionLabel = regexp.MustCompile(`^(?:version|ver|v|final|beta|reset)\s*`)
	// 1 | 1.0 | 1.0.2 | 1,0 | 2.2b | 1.0r1 | 2.7++   (the whole token must match)
	versionToken = regexp.MustCompile(`^(\d+(?:[.,]\d+)*)([a-z]\d*|\++)?$`)
)

type parsedVersion struct {
	parts  []string // numeric components, as written (leading zeros kept: "01" != "1")
	nums   []int
	suffix string
}

func parseVersion(raw string) (parsedVersion, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	for {
		t := versionLabel.ReplaceAllString(s, "")
		if t == s {
			break
		}
		s = t
	}
	// Only the first word counts: "2.0 正式发布版" and "1.1 and 2.1" are 2.0 and 1.1.
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0]
	} else {
		return parsedVersion{}, false
	}
	s = strings.TrimRight(s, ".,")
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, ",") { // ".5" -> "0.5"
		s = "0" + s
	}
	m := versionToken.FindStringSubmatch(s)
	if m == nil {
		return parsedVersion{}, false
	}
	p := parsedVersion{suffix: m[2]}
	for _, c := range strings.Split(strings.ReplaceAll(m[1], ",", "."), ".") {
		n, err := strconv.Atoi(c)
		if err != nil {
			return parsedVersion{}, false
		}
		if n == 0 {
			c = "0" // "00" -> "0"
		}
		p.parts = append(p.parts, c)
		p.nums = append(p.nums, n)
	}
	// "1" -> "1.0", and "1.0.0" / "1.00" -> "1.0": drop trailing zero components but keep
	// at least major.minor, so 1, 1.0, 1.00 and 1.0.0 all become "1.0".
	for len(p.parts) > 2 && p.nums[len(p.parts)-1] == 0 {
		p.parts, p.nums = p.parts[:len(p.parts)-1], p.nums[:len(p.nums)-1]
	}
	for len(p.parts) < 2 {
		p.parts, p.nums = append(p.parts, "0"), append(p.nums, 0)
	}
	return p, true
}

func (p parsedVersion) String() string { return strings.Join(p.parts, ".") + p.suffix }

// NormalizeVersion returns the canonical text of a version, or Unknown when raw isn't one
// ("", "-", "null", "(noVer)", a mod name...). koikatsucards.com shows the same version in
// several ways ("1", "1.0", "v1.0", "1.00", "1.0.0"), and they all become "1.0".
func NormalizeVersion(raw string) string {
	p, ok := parseVersion(raw)
	if !ok {
		return Unknown
	}
	return p.String()
}

// CompareVersions orders two normalized versions: negative when a is older than b. Unknown is
// older than everything, so sorting descending puts it last.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	if a == Unknown {
		return -1
	}
	if b == Unknown {
		return 1
	}
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb { // can't happen for normalized input; stay deterministic anyway
		return strings.Compare(a, b)
	}
	for i := 0; i < len(pa.nums) || i < len(pb.nums); i++ {
		var x, y int
		if i < len(pa.nums) {
			x = pa.nums[i]
		}
		if i < len(pb.nums) {
			y = pb.nums[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	if c := strings.Compare(pa.suffix, pb.suffix); c != 0 { // "2.2" < "2.2b"
		return c
	}
	return strings.Compare(a, b) // "1.01" vs "1.1": same number, different text
}
