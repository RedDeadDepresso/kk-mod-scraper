package scrapers

// Recovery of zip entry names that Pixeldrain returns mangled.
//
// The .zipmod files inside the forum packs are often stored with names in a legacy code page
// (GBK for most, sometimes Shift-JIS, Big5 or EUC-KR) and without the zip "UTF-8" flag.
// Pixeldrain's JSON replaces every byte that is not valid UTF-8 with U+FFFD, one per byte, so
//
//	G11-常服初始版二版.zipmod   (GBK: ... B3 A3 B7 FE B3 F5 CA BC B0 E6 B6 FE B0 E6 ...)
//
// comes back as "G11-" + 6×"�" + "ʼ" + 6×"�" + ".zipmod": "始" is CA BC, which happens to be
// the valid UTF-8 for U+02BC, so it survives as an odd character; the other bytes do not
// form valid UTF-8 and became "�".
//
// The original can't be computed from the mangled name (a "�" has lost its byte), but the
// mangling is deterministic and keeps the byte count. So instead of guessing a pattern, the
// mod list is put through the same transformation: every non-ASCII name of the pack is
// encoded in each legacy code page and decoded as UTF-8 the way Pixeldrain did, and the
// result is compared with the entry name for equality. A mangled name is accepted only if all
// the list names that produce it have the same GUID (several names can collapse to the same
// string, e.g. two short names of two characters: then it is skipped, never guessed).
//
// The download URL keeps the mangled name: Pixeldrain's own URLs for these entries contain
// the U+FFFD bytes (%EF%BF%BD) as well.

import (
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// legacyEncodings are the code pages tried, in this order, for the names of the mod list.
var legacyEncodings = []encoding.Encoding{
	simplifiedchinese.GBK,
	japanese.ShiftJIS,
	traditionalchinese.Big5,
	korean.EUCKR,
}

// mangleBytes decodes b as UTF-8 the way Pixeldrain's (Go) JSON encoder does: each byte that
// is not part of a valid UTF-8 sequence becomes one U+FFFD.
func mangleBytes(b []byte) string {
	out := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			out = append(out, '\uFFFD')
		} else {
			out = append(out, r)
		}
		b = b[n:]
	}
	return string(out)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// mangledForms returns the strings a list name turns into when it is stored in each of the
// legacy code pages and then read as UTF-8 (names that can't be encoded in a code page, or
// that come out unchanged, are skipped).
func mangledForms(name string) []string {
	if isASCII(name) {
		return nil
	}
	var forms []string
	seen := map[string]bool{}
	for _, e := range legacyEncodings {
		b, err := e.NewEncoder().Bytes([]byte(name))
		if err != nil {
			continue
		}
		m := mangleBytes(b)
		if m == name || seen[m] {
			continue
		}
		seen[m] = true
		forms = append(forms, m)
	}
	return forms
}

// mangledIndex returns, for one pack, mangled name -> set of GUIDs. Built on first use.
func (ml *modList) mangledIndex(pack int) map[string]map[string]bool {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	if idx, ok := ml.mangled[pack]; ok {
		return idx
	}
	idx := map[string]map[string]bool{}
	for name, guid := range ml.byPack[pack] {
		for _, m := range mangledForms(name) {
			if idx[m] == nil {
				idx[m] = map[string]bool{}
			}
			idx[m][guid] = true
		}
	}
	if ml.mangled == nil {
		ml.mangled = map[int]map[string]map[string]bool{}
	}
	ml.mangled[pack] = idx
	return idx
}

// lookupMangled finds the GUID of an entry whose name was mangled (see above) inside the
// given packs. It only answers if the mangled name leads to exactly one GUID.
func (ml *modList) lookupMangled(name string, packs []int) string {
	if len(packs) == 0 || isASCII(name) {
		return ""
	}
	guids := map[string]bool{}
	for _, p := range packs {
		for g := range ml.mangledIndex(p)[name] {
			guids[g] = true
		}
	}
	return onlyOne(guids)
}