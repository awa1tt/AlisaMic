package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type diagMsg struct {
	checks []Check
	st     *StationStatus
}

func (m *model) openDiag(from screen) tea.Cmd {
	m.prev = from
	m.scr = scDiag
	m.diagCur = 0
	return m.runDiagCmd()
}

func (m *model) runDiagCmd() tea.Cmd {
	if m.diagBusy {
		return nil
	}
	m.diagBusy = true
	m.diag = nil
	in := diagInput{
		Set:      m.set,
		Cfg:      m.sshCfg(),
		HaveHost: strings.TrimSpace(m.set.SSHHost) != "",
		Outs:     append([]Device(nil), m.outs...),
		Ins:      append([]Device(nil), m.ins...),
		Running:  m.engine.Running(),
		Signal:   m.engine.HasSignal(),
		InLevel:  m.engine.InLevel.Load(),
	}
	if s, ok := m.engine.Sender.Load().(string); ok {
		in.Sender = s
	}
	return func() tea.Msg {
		cs, st := runDiag(in)
		return diagMsg{cs, st}
	}
}

func (m *model) keyDiag(k tea.KeyMsg) tea.Cmd {
	switch normKey(k) {
	case "esc", "q":
		m.scr = m.prev
		if m.scr == scDiag || m.scr == scSplash {
			m.scr = scMain
		}
	case "up", "k", "w":
		if n := len(m.diag); n > 0 {
			m.diagCur = (m.diagCur + n - 1) % n
		}
	case "down", "j", "s", "tab":
		if n := len(m.diag); n > 0 {
			m.diagCur = (m.diagCur + 1) % n
		}
	case "r":
		return m.runDiagCmd()
	case "enter", "f", " ":
		if m.diagBusy || m.diagCur >= len(m.diag) {
			return nil
		}
		c := m.diag[m.diagCur]
		if c.Fix == nil {
			m.flash("для этого пункта автоматического исправления нет", false)
			return nil
		}
		return m.runFix(c)
	}
	return nil
}

// runFix применяет исправление; если у него есть фоновая часть — через окно прогресса.
func (m *model) runFix(c Check) tea.Cmd {
	fix := c.Fix
	run := func() tea.Cmd {
		if fix.Run == nil {
			var cmd tea.Cmd
			if fix.After != nil {
				cmd = fix.After(m)
			}
			m.flash("готово: "+fix.Label, false)
			return tea.Batch(cmd, m.runDiagCmd())
		}
		return m.startOp("Исправление: "+fix.Label, func(l *opLog) opResult {
			if err := fix.Run(l); err != nil {
				return opResult{err: err, next: func(m *model) tea.Cmd { return m.runDiagCmd() }}
			}
			res := opResult{ok: "Готово.", after: fix.After}
			res.next = func(m *model) tea.Cmd {
				if fix.Reboot {
					return tea.Batch(m.askRebootThenDiag())
				}
				return m.runDiagCmd()
			}
			return res
		})
	}
	if len(fix.Confirm) > 0 {
		m.conf = &confirmState{title: fix.Label, lines: fix.Confirm, danger: true, yes: run}
		return nil
	}
	return run()
}

// askRebootThenDiag — после установки предлагаем перезагрузку и перезапускаем диагностику.
func (m *model) askRebootThenDiag() tea.Cmd {
	cfg := m.sshCfg()
	m.conf = &confirmState{title: "Перезагрузить колонку?", danger: true,
		lines: []string{"Изменения вступят в силу после перезагрузки. Алиса будет недоступна 1–2 минуты, программа дождётся её возвращения."},
		yes: func() tea.Cmd {
			return m.startOp("Перезагрузка колонки", func(l *opLog) opResult {
				if err := RebootStation(cfg, l, true); err != nil {
					return opResult{err: err, next: func(m *model) tea.Cmd { return m.runDiagCmd() }}
				}
				return opResult{ok: "Колонка перезагружена.", next: func(m *model) tea.Cmd { return m.runDiagCmd() }}
			})
		}}
	return nil
}

func (m *model) viewDiag() string {
	var b strings.Builder
	status := ""
	switch {
	case m.diagBusy:
		status = sBlue.Render("◌ проверяю…")
	case len(m.diag) > 0:
		bad, warn := 0, 0
		for _, c := range m.diag {
			switch c.St {
			case chkFail:
				bad++
			case chkWarn:
				warn++
			}
		}
		switch {
		case bad > 0:
			status = sBad.Render(fmt.Sprintf("✕ проблем: %d", bad))
		case warn > 0:
			status = sWarn.Render(fmt.Sprintf("! замечаний: %d", warn))
		default:
			status = sOK.Render("✓ всё в порядке")
		}
	}
	b.WriteString(m.headerWith("Диагностика", status) + "\n" + rule() + "\n")

	if m.diagBusy {
		b.WriteString("\n" + sDim.Render("Проверяю ПК, колонку и приём звука (до ~6 секунд)…") + "\n")
		for i := 0; i < 15; i++ {
			b.WriteString("\n")
		}
	} else {
		group := ""
		for i, c := range m.diag {
			if c.Group != group {
				group = c.Group
				b.WriteString(sDim.Render("  "+strings.ToUpper(group)) + "\n")
			}
			mark := sDim.Render("–")
			switch c.St {
			case chkOK:
				mark = sOK.Render("✓")
			case chkWarn:
				mark = sWarn.Render("!")
			case chkFail:
				mark = sBad.Render("✕")
			}
			fixMark := " "
			if c.Fix != nil {
				fixMark = sPurple.Render("⚒")
			}
			line := mark + " " + padRight(truncate(c.Name, 19), 19) + sDim.Render(truncate(c.Detail, panelWidth-26))
			if i == m.diagCur {
				b.WriteString(sPurple.Render("› ") + mark + " " + sSelected.Render(padRight(truncate(c.Name, 19), 19)) + sDim.Render(truncate(c.Detail, panelWidth-26)))
			} else {
				b.WriteString("  " + line)
			}
			b.WriteString(" " + fixMark + "\n")
		}
	}
	b.WriteString(rule() + "\n")
	b.WriteString(m.diagDetail())
	b.WriteString(rule() + "\n")
	if m.msg != "" {
		b.WriteString(m.msgOrHint("") + "\n")
	}
	b.WriteString(keyHint("↑↓", "выбор") + "   " + keyHint("enter", "исправить ⚒") + "   " + keyHint("r", "заново") + "   " + keyHint("esc", "назад"))
	return b.String()
}

// diagDetail — подробности выбранного пункта (4 строки).
func (m *model) diagDetail() string {
	lines := []string{"", "", "", ""}
	if !m.diagBusy && m.diagCur < len(m.diag) {
		c := m.diag[m.diagCur]
		wrapped := wrapLines([]string{c.Detail}, panelWidth)
		for i := 0; i < 3 && i < len(wrapped); i++ {
			lines[i] = sDim.Render(wrapped[i])
		}
		if len(wrapped) > 3 {
			lines[2] = sDim.Render(truncate(wrapped[2], panelWidth-1) + "…")
		}
		if c.Fix != nil {
			lines[3] = sPurple.Render("⚒ Enter — " + truncate(c.Fix.Label, panelWidth-12))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
