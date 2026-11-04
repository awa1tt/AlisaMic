package main

// Палитра: только синий и фиолетовый (как на иконке) + приглушённый серый.

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type rgb struct{ r, g, b float64 }

var (
	cBlue   = rgb{0x5B, 0x6C, 0xFF}
	cPurple = rgb{0xB4, 0x5C, 0xFF}
	cDim    = rgb{0x6E, 0x6A, 0x86}

	// цвета статусов — только для галочек диагностики
	cOK   = rgb{0x6E, 0xE7, 0xA8}
	cWarn = rgb{0xFF, 0xC8, 0x57}
	cBad  = rgb{0xFF, 0x6B, 0x8A}
)

func (c rgb) hex() lipgloss.Color {
	return lipgloss.Color(fmt.Sprintf("#%02X%02X%02X", int(c.r), int(c.g), int(c.b)))
}

func mix(a, b rgb, t float64) rgb {
	t = clampF(t, 0, 1)
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}

var (
	sBlue     = lipgloss.NewStyle().Foreground(cBlue.hex())
	sPurple   = lipgloss.NewStyle().Foreground(cPurple.hex())
	sDim      = lipgloss.NewStyle().Foreground(cDim.hex())
	sTitle    = lipgloss.NewStyle().Foreground(cPurple.hex()).Bold(true)
	sSelected = lipgloss.NewStyle().Foreground(cPurple.hex()).Bold(true)
	sValue    = lipgloss.NewStyle().Foreground(cBlue.hex())
	sKey      = lipgloss.NewStyle().Foreground(cBlue.hex()).Bold(true)
	sOK       = lipgloss.NewStyle().Foreground(cOK.hex()).Bold(true)
	sWarn     = lipgloss.NewStyle().Foreground(cWarn.hex()).Bold(true)
	sBad      = lipgloss.NewStyle().Foreground(cBad.hex()).Bold(true)
)

// gradientText — текст с плавным переходом синий → фиолетовый.
func gradientText(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		t := 0.0
		if len(runes) > 1 {
			t = float64(i) / float64(len(runes)-1)
		}
		b.WriteString(lipgloss.NewStyle().Foreground(mix(cBlue, cPurple, t).hex()).Render(string(r)))
	}
	return b.String()
}

// meter — полоска уровня в дБ (−60…0 дБFS).
func meter(rms float64, width int, active bool) string {
	frac := 0.0
	if rms > 0 {
		frac = clampF((20*math.Log10(rms/32768)+60)/60, 0, 1)
	}
	filled := int(math.Round(frac * float64(width)))
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i < filled && active {
			t := float64(i) / float64(width-1)
			b.WriteString(lipgloss.NewStyle().Foreground(mix(cBlue, cPurple, t).hex()).Render("━"))
		} else {
			b.WriteString(sDim.Render("─"))
		}
	}
	return b.String()
}
