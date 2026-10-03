package embed

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// WordPiece is a BERT-style uncased tokenizer (BertNormalizer +
// BertPreTokenizer + WordPiece), bit-compatible with HuggingFace tokenizers
// for the configuration Model2Vec ships.
type WordPiece struct {
	vocab        map[string]int32
	unk          int32
	prefix       string
	maxWordChars int
}

type tokenizerJSON struct {
	Model struct {
		Type                    string           `json:"type"`
		UnkToken                string           `json:"unk_token"`
		ContinuingSubwordPrefix string           `json:"continuing_subword_prefix"`
		MaxInputCharsPerWord    int              `json:"max_input_chars_per_word"`
		Vocab                   map[string]int32 `json:"vocab"`
	} `json:"model"`
}

// LoadWordPiece parses a HuggingFace tokenizer.json with a WordPiece model.
func LoadWordPiece(path string) (*WordPiece, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tj tokenizerJSON
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, fmt.Errorf("parse tokenizer: %w", err)
	}
	if tj.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("tokenizer model %q unsupported (want WordPiece)", tj.Model.Type)
	}
	unk, ok := tj.Model.Vocab[tj.Model.UnkToken]
	if !ok {
		unk = -1
	}
	wp := &WordPiece{
		vocab:        tj.Model.Vocab,
		unk:          unk,
		prefix:       tj.Model.ContinuingSubwordPrefix,
		maxWordChars: tj.Model.MaxInputCharsPerWord,
	}
	if wp.prefix == "" {
		wp.prefix = "##"
	}
	if wp.maxWordChars <= 0 {
		wp.maxWordChars = 100
	}
	return wp, nil
}

// VocabSize returns the number of vocabulary entries.
func (w *WordPiece) VocabSize() int { return len(w.vocab) }

// Encode tokenizes text and calls fn for each token id, skipping unknown
// tokens (Model2Vec drops [UNK]). It allocates only the normalized buffer.
func (w *WordPiece) Encode(text string, fn func(id int32)) {
	norm := normalize(text)
	// Pre-tokenize: whitespace split + isolate punctuation.
	start := -1
	for i, r := range norm {
		switch {
		case r == ' ':
			if start >= 0 {
				w.word(norm[start:i], fn)
				start = -1
			}
		case isPunct(r):
			if start >= 0 {
				w.word(norm[start:i], fn)
				start = -1
			}
			w.word(norm[i:i+utf8.RuneLen(r)], fn)
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		w.word(norm[start:], fn)
	}
}

// IDs returns token ids (testing / debugging helper).
func (w *WordPiece) IDs(text string) []int32 {
	var out []int32
	w.Encode(text, func(id int32) { out = append(out, id) })
	return out
}

func (w *WordPiece) word(word string, fn func(id int32)) {
	if utf8.RuneCountInString(word) > w.maxWordChars {
		return // whole word → [UNK], dropped
	}
	if id, ok := w.vocab[word]; ok {
		if id != w.unk {
			fn(id)
		}
		return
	}
	// Greedy longest-match-first. Collect ids first: if any piece fails the
	// whole word is [UNK] and must emit nothing.
	var buf [32]int32
	ids := buf[:0]
	var key strings.Builder
	startB := 0
	for startB < len(word) {
		endB := len(word)
		found := int32(-1)
		for endB > startB {
			sub := word[startB:endB]
			var id int32
			var ok bool
			if startB > 0 {
				key.Reset()
				key.WriteString(w.prefix)
				key.WriteString(sub)
				id, ok = w.vocab[key.String()]
			} else {
				id, ok = w.vocab[sub]
			}
			if ok {
				found = id
				break
			}
			_, sz := utf8.DecodeLastRuneInString(sub)
			endB -= sz
		}
		if found < 0 {
			return
		}
		ids = append(ids, found)
		startB = endB
	}
	for _, id := range ids {
		if id != w.unk {
			fn(id)
		}
	}
}

// normalize applies BertNormalizer(clean_text, handle_chinese_chars,
// strip_accents=lowercase, lowercase).
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c == '\t' || c == '\n' || c == '\r':
				b.WriteByte(' ')
			case c < 0x20 || c == 0x7f:
				// control: drop
			case c >= 'A' && c <= 'Z':
				b.WriteByte(c + 32)
			default:
				b.WriteByte(c)
			}
		}
		return b.String()
	}
	// Unicode path: clean + chinese padding, then NFD + strip Mn + lowercase.
	var pre strings.Builder
	pre.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == 0 || r == utf8.RuneError || isControl(r):
			continue
		case isWhitespace(r):
			pre.WriteByte(' ')
		case isCJK(r):
			pre.WriteByte(' ')
			pre.WriteRune(r)
			pre.WriteByte(' ')
		default:
			pre.WriteRune(r)
		}
	}
	d := norm.NFD.String(pre.String())
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

func isWhitespace(r rune) bool {
	if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs)
}

func isPunct(r rune) bool {
	if r < 128 {
		return (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126)
	}
	return unicode.IsPunct(r)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}
