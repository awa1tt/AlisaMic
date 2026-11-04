package main

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// field — однострочный редактор текста (с курсором, вставкой из буфера и маской для паролей).
type field struct {
	buf    []rune
	pos    int
	mask   bool
	digits bool
	max    int
}

func newField(val string, mask, digits bool, max int) *field {
	f := &field{mask: mask, digits: digits, max: max}
	f.buf = []rune(val)
	f.pos = len(f.buf)
	return f
}

func (f *field) String() string { return string(f.buf) }

func (f *field) insert(r rune) {
	if r == '\n' || r == '\r' || r == '\t' || (r < 32 && r != ' ') {
		return
	}
	if f.digits && !unicode.IsDigit(r) {
		return
	}
	if f.max > 0 && len(f.buf) >= f.max {
		return
	}
	f.buf = append(f.buf, 0)
	copy(f.buf[f.pos+1:], f.buf[f.pos:])
	f.buf[f.pos] = r
	f.pos++
}

func (f *field) handle(k tea.KeyMsg) {
	switch k.Type {
	case tea.KeyRunes:
		if k.Alt {
			return
		}
		for _, r := range k.Runes {
			f.insert(r)
		}
	case tea.KeySpace:
		f.insert(' ')
	case tea.KeyBackspace:
		if f.pos > 0 {
			f.buf = append(f.buf[:f.pos-1], f.buf[f.pos:]...)
			f.pos--
		}
	case tea.KeyDelete:
		if f.pos < len(f.buf) {
			f.buf = append(f.buf[:f.pos], f.buf[f.pos+1:]...)
		}
	case tea.KeyLeft:
		if f.pos > 0 {
			f.pos--
		}
	case tea.KeyRight:
		if f.pos < len(f.buf) {
			f.pos++
		}
	case tea.KeyHome, tea.KeyCtrlA:
		f.pos = 0
	case tea.KeyEnd, tea.KeyCtrlE:
		f.pos = len(f.buf)
	case tea.KeyCtrlU:
		f.buf, f.pos = nil, 0
	}
}

func (f *field) view() string {
	shown := f.buf
	if f.mask {
		shown = []rune(strings.Repeat("•", len(f.buf)))
	}
	return sValue.Render(string(shown[:f.pos])) + sPurple.Render("▏") + sValue.Render(string(shown[f.pos:]))
}

// Русская раскладка → клавиши, на которых держатся горячие клавиши (ЙЦУКЕН ↔ QWERTY).
var ruKeys = map[string]string{
	"й": "q", "ц": "w", "у": "e", "к": "r", "е": "t", "н": "y", "г": "u", "ш": "i", "щ": "o", "з": "p",
	"ф": "a", "ы": "s", "в": "d", "а": "f", "п": "g", "р": "h", "о": "j", "л": "k", "д": "l",
	"я": "z", "ч": "x", "с": "c", "м": "v", "и": "b", "т": "n", "ь": "m",
}

// normKey приводит нажатие к латинскому виду: «Й» и «й» → «q».
func normKey(k tea.KeyMsg) string {
	s := k.String()
	if len([]rune(s)) == 1 {
		s = strings.ToLower(s)
		if v, ok := ruKeys[s]; ok {
			return v
		}
	}
	return s
}
