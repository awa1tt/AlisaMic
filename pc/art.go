package main

import (
	_ "embed"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

//go:embed ascii.txt
var asciiArt string

// renderArt раскрашивает арт по диагонали, как на иконке:
// сверху справа — синий, снизу слева — фиолетовый. Фон (░) приглушён, лицо (█) светлее.
func renderArt() (lines []string, width int) {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(asciiArt, "\r", ""), "\n"), "\n")
	for len(raw) > 0 && strings.TrimSpace(raw[0]) == "" {
		raw = raw[1:]
	}
	for _, l := range raw {
		if n := len([]rune(l)); n > width {
			width = n
		}
	}
	h := float64(len(raw))
	black := rgb{0x0B, 0x08, 0x18}
	white := rgb{0xF2, 0xEC, 0xFF}

	for y, l := range raw {
		var b strings.Builder
		runes := []rune(l)
		// пачки одинаковых символов красим одним стилем — меньше escape-кодов
		for x := 0; x < len(runes); {
			r := runes[x]
			if r == ' ' {
				b.WriteRune(' ')
				x++
				continue
			}
			end := x
			for end < len(runes) && runes[end] == r && end-x < 4 {
				end++
			}
			t := (float64(width-x)/float64(width) + float64(y)/h) / 2
			c := mix(cBlue, cPurple, t)
			switch r {
			case '░':
				c = mix(c, black, 0.3)
			case '▒':
				c = mix(c, black, 0.15)
			case '▓':
				c = mix(c, white, 0.35)
			case '█':
				c = mix(c, white, 0.75)
			}
			b.WriteString(lipgloss.NewStyle().Foreground(c.hex()).Render(string(runes[x:end])))
			x = end
		}
		// дополняем пробелами до общей ширины, чтобы центрирование было ровным
		b.WriteString(strings.Repeat(" ", width-len(runes)))
		lines = append(lines, b.String())
	}
	return lines, width
}
