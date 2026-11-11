package inline

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// exportOpen and exportClose wrap auto-exported functions. They are
// inserted on existing lines, so line numbers in diagnostics stay exact.
const (
	exportOpen  = "ferrogo::export! { "
	exportClose = " }"
)

// AutoExport wraps every top-level `pub fn` item (with its doc comments and
// attributes) in ferrogo::export!, so plain Rust functions become callable
// from Go:
//
//	pub fn add(a: i32, b: i32) -> i32 { a + b }
//
// Items that are not exactly `pub fn` (helpers without pub, pub(crate),
// pub async/unsafe/const/extern fn, methods, nested modules) are left alone,
// as is code already inside a ferrogo::export! invocation.
func AutoExport(src string) string {
	spans := exportSpans(src)
	if len(spans) == 0 {
		return src
	}
	var b strings.Builder
	b.Grow(len(src) + len(spans)*(len(exportOpen)+len(exportClose)))
	prev := 0
	for _, s := range spans {
		b.WriteString(src[prev:s[0]])
		b.WriteString(exportOpen)
		b.WriteString(src[s[0]:s[1]])
		b.WriteString(exportClose)
		prev = s[1]
	}
	b.WriteString(src[prev:])
	return b.String()
}

// exportSpans returns [start, end) byte ranges of top-level `pub fn` items.
func exportSpans(src string) [][2]int {
	lx := lexer{src: src}
	var spans [][2]int
	depth := 0
	itemStart := -1 // start of the current top-level item, including attributes
	candidate := false
	for {
		tok, start, end := lx.next()
		if tok == tokEOF {
			break
		}
		if depth == 0 && itemStart < 0 && tok != tokCloseBrace {
			itemStart = start
			candidate = false
		}
		switch tok {
		case tokOpen:
			depth++
		case tokClose:
			if depth > 0 {
				depth--
			}
		case tokCloseBrace:
			if depth > 0 {
				depth--
			}
			if depth == 0 {
				if candidate {
					spans = append(spans, [2]int{itemStart, end})
				}
				itemStart, candidate = -1, false
			}
		case tokSemicolon:
			if depth == 0 {
				itemStart, candidate = -1, false
			}
		case tokIdent:
			if depth == 0 && src[start:end] == "pub" && !candidate {
				// `pub` immediately followed by `fn`?
				save := lx.pos
				if t, s, e := lx.next(); t == tokIdent && src[s:e] == "fn" {
					candidate = true
				}
				lx.pos = save
			}
		case tokInnerAttr:
			if depth == 0 {
				itemStart = -1 // #![...] belongs to the crate, not an item
			}
		}
	}
	return spans
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokOpen       // ( [ {
	tokClose      // ) ]
	tokCloseBrace // } (only a brace returning to depth 0 ends an item)
	tokSemicolon
	tokInnerAttr // #![
	tokOther     // punctuation, literals, doc comments, attributes' '#'
)

// lexer is a minimal Rust tokenizer: it skips whitespace and comments and
// treats string, char and raw-string literals as opaque, which is all that
// is needed to find item boundaries reliably.
type lexer struct {
	src string
	pos int
}

func (l *lexer) peek(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

func (l *lexer) next() (tokKind, int, int) {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		start := l.pos
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case c == '/' && l.peek(1) == '/':
			doc := l.peek(2) == '/' && l.peek(3) != '/' || l.peek(2) == '!'
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			if doc {
				return tokOther, start, l.pos // doc comments attach to items
			}
		case c == '/' && l.peek(1) == '*':
			doc := (l.peek(2) == '*' && l.peek(3) != '*' && l.peek(3) != '/') || l.peek(2) == '!'
			l.skipBlockComment()
			if doc {
				return tokOther, start, l.pos
			}
		case c == '{' || c == '(' || c == '[':
			l.pos++
			return tokOpen, start, l.pos
		case c == ')' || c == ']':
			l.pos++
			return tokClose, start, l.pos
		case c == '}':
			l.pos++
			return tokCloseBrace, start, l.pos
		case c == ';':
			l.pos++
			return tokSemicolon, start, l.pos
		case c == '#' && l.peek(1) == '!' && l.peek(2) == '[':
			l.pos += 3
			return tokInnerAttr, start, l.pos
		case c == '"':
			l.skipString()
			return tokOther, start, l.pos
		case c == '\'':
			l.skipCharOrLifetime()
			return tokOther, start, l.pos
		case (c == 'r' || c == 'b') && l.rawOrByteString():
			return tokOther, start, l.pos
		case c == '_' || c < utf8.RuneSelf && unicode.IsLetter(rune(c)) || c >= utf8.RuneSelf:
			for l.pos < len(l.src) {
				r, size := utf8.DecodeRuneInString(l.src[l.pos:])
				if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					break
				}
				l.pos += size
			}
			if l.pos == start { // a non-identifier multi-byte rune
				_, size := utf8.DecodeRuneInString(l.src[l.pos:])
				l.pos += size
				return tokOther, start, l.pos
			}
			return tokIdent, start, l.pos
		default:
			l.pos++
			return tokOther, start, l.pos
		}
	}
	return tokEOF, l.pos, l.pos
}

func (l *lexer) skipBlockComment() {
	l.pos += 2
	for depth := 1; l.pos < len(l.src) && depth > 0; {
		switch {
		case l.src[l.pos] == '/' && l.peek(1) == '*':
			depth++
			l.pos += 2
		case l.src[l.pos] == '*' && l.peek(1) == '/':
			depth--
			l.pos += 2
		default:
			l.pos++
		}
	}
}

func (l *lexer) skipString() {
	l.pos++ // opening quote
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case '\\':
			l.pos += 2
		case '"':
			l.pos++
			return
		default:
			l.pos++
		}
	}
}

// skipCharOrLifetime distinguishes 'a' / '\n' (char literals) from 'a
// (lifetimes and loop labels).
func (l *lexer) skipCharOrLifetime() {
	l.pos++ // opening quote
	if l.peek(0) == '\\' {
		l.pos += 2
		for l.pos < len(l.src) && l.src[l.pos] != '\'' {
			l.pos++ // \u{...} and similar
		}
		l.pos++
		return
	}
	_, size := utf8.DecodeRuneInString(l.src[l.pos:])
	if l.pos+size < len(l.src) && l.src[l.pos+size] == '\'' {
		l.pos += size + 1 // char literal
	}
	// Otherwise a lifetime: the identifier is lexed as the next token.
}

// rawOrByteString consumes r"..", r#".."#, b"..", br".." and b'..' literals.
func (l *lexer) rawOrByteString() bool {
	i := l.pos
	if l.src[i] == 'b' {
		i++
		if i < len(l.src) && l.src[i] == '\'' {
			l.pos = i
			l.skipCharOrLifetime()
			return true
		}
		if i < len(l.src) && l.src[i] == '"' {
			l.pos = i
			l.skipString()
			return true
		}
		if i >= len(l.src) || l.src[i] != 'r' {
			return false
		}
	}
	i++ // past 'r'
	hashes := 0
	for i < len(l.src) && l.src[i] == '#' {
		hashes++
		i++
	}
	if i >= len(l.src) || l.src[i] != '"' {
		return false
	}
	closing := "\"" + strings.Repeat("#", hashes)
	end := strings.Index(l.src[i+1:], closing)
	if end < 0 {
		l.pos = len(l.src)
	} else {
		l.pos = i + 1 + end + len(closing)
	}
	return true
}

// DocComments returns the `///` documentation of functions declared at the
// top level or directly inside a ferrogo::export! block, keyed by name.
func DocComments(src string) map[string]string {
	docs := map[string]string{}
	lx := lexer{src: src}
	depth := 0
	var pending []string
	for {
		tok, start, end := lx.next()
		if tok == tokEOF {
			return docs
		}
		text := src[start:end]
		switch {
		case tok == tokOther && strings.HasPrefix(text, "///"):
			pending = append(pending, strings.TrimPrefix(strings.TrimPrefix(text, "///"), " "))
			continue
		case tok == tokOpen:
			depth++
		case tok == tokClose || tok == tokCloseBrace:
			depth = max(0, depth-1)
		case tok == tokIdent && text == "fn" && depth <= 1:
			save := lx.pos
			if t, s, e := lx.next(); t == tokIdent && len(pending) > 0 {
				if _, seen := docs[src[s:e]]; !seen {
					docs[src[s:e]] = strings.TrimSpace(strings.Join(pending, "\n"))
				}
			}
			lx.pos = save
		case tok == tokOther && text == "#":
			// Skip an outer attribute (#[inline]) as a unit: it sits between
			// the docs and the fn and must not clear them.
			if t, s, _ := lx.next(); t == tokOpen && src[s] == '[' {
				for nest := 1; nest > 0; {
					t, _, _ := lx.next()
					switch t {
					case tokOpen:
						nest++
					case tokClose, tokCloseBrace:
						nest--
					case tokEOF:
						return docs
					}
				}
			}
			continue
		case tok == tokIdent:
			continue // qualifiers such as pub keep pending docs
		}
		if tok != tokIdent {
			pending = nil
		}
	}
}
