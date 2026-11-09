package scrapers

// Recovering Gofile's signing secret from the script that holds it.
//
// Adapted from HeapLeach (https://github.com/JohanLindvall/HeapLeach, MIT licence),
// backend/internal/extractor/gofilewt.go.
//
// Every content request to Gofile's API is signed with a secret baked into its
// obfuscated script /js/wt.obf.js, and Gofile rotates it. The script is
// javascript-obfuscator output: its strings sit in a table, each RC4-encrypted under a
// per-call-site key and base64-encoded with a shuffled alphabet, and the table is rotated
// by a startup loop. Undoing that needs no JavaScript interpreter, only the constants the
// file carries in the clear: the alphabet, the table and the call sites (index + key), and
// a search for the one rotation at which every call site decodes to readable text.
//
// Gofile blocks addresses that sign badly, so the extraction refuses to guess: it only
// answers when the whole file decodes and exactly one decoded string has the shape of a
// secret. Otherwise the caller falls back to the pinned/configured secret.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// gofileScriptPath is where the site loads its signing code from.
const gofileScriptPath = "/js/wt.obf.js"

var (
	gofileAlphabetPattern  = regexp.MustCompile(`'([A-Za-z0-9+/]{64}=)'`)
	gofileTablePattern     = regexp.MustCompile(`\[((?:'[A-Za-z0-9+/=]*'\s*,\s*){8,}'[A-Za-z0-9+/=]*')\]`)
	gofileTableItemPattern = regexp.MustCompile(`'([A-Za-z0-9+/=]*)'`)
	gofileCallPattern      = regexp.MustCompile(`\(\s*0x([0-9a-fA-F]+)\s*,\s*'([^']{1,32})'\s*\)`)
	gofileHexEscape        = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)

	// gofileSecretShape is what a secret looks like: hex, and long enough not to be a
	// counter or a colour.
	gofileSecretShape = regexp.MustCompile(`^[0-9a-f]{10,40}$`)
)

// gofileSecretFrom extracts the secret from a fetched script. Kept apart from the fetch so
// it can be tested against a script instead of the site.
func gofileSecretFrom(script string) (string, error) {
	script = gofileUnescape(script)

	m := gofileAlphabetPattern.FindStringSubmatch(script)
	if m == nil {
		return "", fmt.Errorf("no base64 alphabet in the signing script")
	}
	alphabet := m[1]
	table := gofileTable(script)
	if len(table) == 0 {
		return "", fmt.Errorf("no string table in the signing script")
	}
	calls := gofileCalls(script)
	if len(calls) == 0 {
		return "", fmt.Errorf("no decodable references in the signing script")
	}
	decoded, ok := gofileDecodeAll(table, calls, alphabet)
	if !ok {
		return "", fmt.Errorf("the signing script did not decode; its obfuscation has changed")
	}
	return gofilePickSecret(decoded)
}

// gofileTable finds the encoded strings, taking the longest table in the file.
func gofileTable(script string) []string {
	var table []string
	for _, m := range gofileTablePattern.FindAllStringSubmatch(script, -1) {
		items := gofileTableItemPattern.FindAllStringSubmatch(m[1], -1)
		if len(items) <= len(table) {
			continue
		}
		table = table[:0]
		for _, item := range items {
			table = append(table, item[1])
		}
	}
	return table
}

// gofileCall is one reference to the table: an index and the key its entry was encrypted under.
type gofileCall struct {
	index int
	key   string
}

func gofileCalls(script string) []gofileCall {
	var calls []gofileCall
	seen := map[gofileCall]bool{}
	for _, m := range gofileCallPattern.FindAllStringSubmatch(script, -1) {
		index, err := strconv.ParseInt(m[1], 16, 32)
		if err != nil {
			continue
		}
		call := gofileCall{index: int(index), key: m[2]}
		if !seen[call] {
			seen[call] = true
			calls = append(calls, call)
		}
	}
	return calls
}

// gofileDecodeAll finds the table's rotation and decodes every reference. The rotation and
// the decoder's index offset only ever appear added together, so there are just as many
// possible sums as table entries; the right one is the offset at which every reference
// decodes to printable text.
func gofileDecodeAll(table []string, calls []gofileCall, alphabet string) ([]string, bool) {
	for offset := range table {
		decoded := make([]string, 0, len(calls))
		ok := true
		for _, call := range calls {
			entry := table[((call.index+offset)%len(table)+len(table))%len(table)]
			text, err := gofileDecodeEntry(entry, call.key, alphabet)
			if err != nil || !gofilePrintable(text) {
				ok = false
				break
			}
			decoded = append(decoded, text)
		}
		if ok {
			return decoded, true
		}
	}
	return nil, false
}

// gofileDecodeEntry undoes one entry: base64 under the shuffled alphabet, read as UTF-8
// (the script does it in that order), then RC4 over characters under the call site's key.
func gofileDecodeEntry(encoded, key, alphabet string) (string, error) {
	raw, err := gofileBase64(encoded, alphabet)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("entry is not text")
	}
	return gofileRC4Runes(string(raw), key), nil
}

// gofileBase64 decodes under a shuffled alphabet, six bits at a time as the script does.
func gofileBase64(encoded, alphabet string) ([]byte, error) {
	out := make([]byte, 0, len(encoded)*3/4)
	acc, bits := 0, 0
	for i := 0; i < len(encoded); i++ {
		value := strings.IndexByte(alphabet, encoded[i])
		if value < 0 || value >= 64 {
			continue // padding, or a character this alphabet does not use
		}
		acc = acc<<6 | value
		if bits += 6; bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("entry decoded to nothing")
	}
	return out, nil
}

// gofileRC4Runes enciphers text against a key, over characters rather than bytes.
func gofileRC4Runes(text, key string) string {
	keyRunes := []rune(key)
	if len(keyRunes) == 0 {
		return ""
	}
	var state [256]byte
	for i := range state {
		state[i] = byte(i)
	}
	for i, j := 0, 0; i < 256; i++ {
		j = (j + int(state[i]) + int(keyRunes[i%len(keyRunes)])) % 256
		state[i], state[j] = state[j], state[i]
	}
	var out strings.Builder
	out.Grow(len(text))
	i, j := 0, 0
	for _, r := range text {
		i = (i + 1) % 256
		j = (j + int(state[i])) % 256
		state[i], state[j] = state[j], state[i]
		out.WriteRune(r ^ rune(state[(int(state[i])+int(state[j]))%256]))
	}
	return out.String()
}

// gofilePickSecret refuses to choose between candidates: one string of the right shape is
// an answer, several is a guess.
func gofilePickSecret(decoded []string) (string, error) {
	seen := map[string]bool{}
	var candidates []string
	for _, text := range decoded {
		if !gofileSecretShape.MatchString(text) || seen[text] || gofileUniform(text) {
			continue
		}
		seen[text] = true
		candidates = append(candidates, text)
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return "", fmt.Errorf("the signing script holds nothing shaped like a secret")
	default:
		return "", fmt.Errorf("the signing script holds %d strings shaped like a secret, so which one signs is a guess", len(candidates))
	}
}

// gofileUniform reports a string of one repeated character (a placeholder).
func gofileUniform(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}

func gofilePrintable(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < 0x20 || text[i] > 0x7e {
			return false
		}
	}
	return true
}

// gofileUnescape undoes the script's \xNN escapes.
func gofileUnescape(script string) string {
	return gofileHexEscape.ReplaceAllStringFunc(script, func(esc string) string {
		b, _ := strconv.ParseUint(esc[2:], 16, 8)
		return string(rune(b))
	})
}
