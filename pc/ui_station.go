package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ───────────────────────────── модальные окна ─────────────────────────────

type busyState struct {
	title   string
	log     *opLog
	started time.Time
}

type resultState struct {
	title string
	lines []string
	err   error
	ok    string
	next  func(m *model) tea.Cmd
}

type promptState struct {
	title, label, hint string
	f                  *field
	validate           func(string) error
	submit             func(string) tea.Cmd
	err                string
}

type confirmState struct {
	title  string
	lines  []string
	danger bool
	yes    func() tea.Cmd
}

type opResult struct {
	err   error
	ok    string
	after func(m *model) tea.Cmd // на UI-потоке сразу по завершении
	next  func(m *model) tea.Cmd // после закрытия окна результата
	// silent: при успехе окно результата не показывать, сразу выполнить next
	silent bool
}

type opDoneMsg struct {
	id    int
	title string
	lines []string
	res   opResult
}

type statusMsg struct {
	st  *StationStatus
	err error
}

// startOp запускает долгую операцию в фоне и показывает окно прогресса.
func (m *model) startOp(title string, fn func(l *opLog) opResult) tea.Cmd {
	m.opID++
	id := m.opID
	l := &opLog{}
	m.busy = &busyState{title: title, log: l, started: time.Now()}
	return func() tea.Msg {
		var res opResult
		func() {
			defer func() {
				if r := recover(); r != nil {
					res = opResult{err: fmt.Errorf("внутренняя ошибка: %v", r)}
				}
			}()
			res = fn(l)
		}()
		return opDoneMsg{id: id, title: title, lines: l.snapshot(), res: res}
	}
}

func (m *model) onOpDone(msg opDoneMsg) tea.Cmd {
	if msg.id != m.opID {
		return nil
	}
	m.busy = nil
	var cmd tea.Cmd
	if msg.res.after != nil {
		cmd = msg.res.after(m)
	}
	if msg.res.silent && msg.res.err == nil {
		if msg.res.next != nil {
			return tea.Batch(cmd, msg.res.next(m))
		}
		return cmd
	}
	m.res = &resultState{title: msg.title, lines: msg.lines, err: msg.res.err, ok: msg.res.ok, next: msg.res.next}
	return cmd
}

func (m *model) keyResult(k tea.KeyMsg) tea.Cmd {
	switch normKey(k) {
	case "enter", "esc", " ", "q":
		r := m.res
		m.res = nil
		if r.next != nil {
			return r.next(m)
		}
	}
	return nil
}

func (m *model) keyPrompt(k tea.KeyMsg) tea.Cmd {
	p := m.prompt
	switch k.String() {
	case "esc":
		m.prompt = nil
	case "enter":
		v := p.f.String()
		if p.validate != nil {
			if err := p.validate(v); err != nil {
				p.err = err.Error()
				return nil
			}
		}
		m.prompt = nil
		return p.submit(v)
	default:
		p.err = ""
		p.f.handle(k)
	}
	return nil
}

func (m *model) keyConfirm(k tea.KeyMsg) tea.Cmd {
	c := m.conf
	switch key := normKey(k); key {
	case "y":
		m.conf = nil
		return c.yes()
	case "enter":
		if !c.danger {
			m.conf = nil
			return c.yes()
		}
	case "n", "esc", "q":
		m.conf = nil
	}
	return nil
}

var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func wrapLines(lines []string, width int) []string {
	var out []string
	st := lipgloss.NewStyle().Width(width)
	for _, l := range lines {
		for _, w := range strings.Split(st.Render(l), "\n") {
			out = append(out, strings.TrimRight(w, " "))
		}
	}
	return out
}

func indentLines(lines []string, pad string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = pad + l
	}
	return out
}

func lastN(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func (m *model) viewBusy() string {
	b := m.busy
	var s strings.Builder
	frame := spinFrames[int(time.Since(b.started)/(80*time.Millisecond))%len(spinFrames)]
	s.WriteString(gradientText(b.title) + "\n" + rule() + "\n\n")
	s.WriteString(sPurple.Render(frame) + " " + sValue.Render("выполняется…") + sDim.Render(fmt.Sprintf("  %d с", int(time.Since(b.started).Seconds()))) + "\n\n")
	lines := lastN(indentLines(wrapLines(b.log.snapshot(), panelWidth-2), "  "), 14)
	for _, l := range lines {
		s.WriteString(sDim.Render(l) + "\n")
	}
	for i := len(lines); i < 14; i++ {
		s.WriteString("\n")
	}
	s.WriteString("\n" + rule() + "\n" + sDim.Render("Не закрывайте программу и не отключайте питание колонки"))
	return s.String()
}

func (m *model) viewResult() string {
	r := m.res
	var s strings.Builder
	mark := sOK.Render("✓ ")
	if r.err != nil {
		mark = sBad.Render("✕ ")
	}
	s.WriteString(mark + gradientText(r.title) + "\n" + rule() + "\n\n")
	lines := lastN(indentLines(wrapLines(r.lines, panelWidth-2), "  "), 12)
	for _, l := range lines {
		s.WriteString(sDim.Render(l) + "\n")
	}
	if len(lines) > 0 {
		s.WriteString("\n")
	}
	if r.err != nil {
		for _, l := range indentLines(wrapLines([]string{r.err.Error()}, panelWidth-2), "  ") {
			s.WriteString(sBad.Render(l) + "\n")
		}
	} else if r.ok != "" {
		for _, l := range indentLines(wrapLines([]string{r.ok}, panelWidth-2), "  ") {
			s.WriteString(sOK.Render(l) + "\n")
		}
	}
	s.WriteString("\n" + rule() + "\n" + keyHint("enter", "закрыть"))
	return s.String()
}

func (m *model) viewPrompt() string {
	p := m.prompt
	var s strings.Builder
	s.WriteString(gradientText(p.title) + "\n" + rule() + "\n\n")
	s.WriteString(sDim.Render(p.label) + "\n\n  " + p.f.view() + "\n\n")
	if p.err != "" {
		s.WriteString(sBad.Render("✕ "+p.err) + "\n")
	} else if p.hint != "" {
		for _, l := range wrapLines([]string{p.hint}, panelWidth) {
			s.WriteString(sDim.Render(l) + "\n")
		}
	}
	s.WriteString("\n" + rule() + "\n" + keyHint("enter", "дальше") + "   " + keyHint("esc", "отмена"))
	return s.String()
}

func (m *model) viewConfirm() string {
	c := m.conf
	var s strings.Builder
	title := gradientText(c.title)
	if c.danger {
		title = sBad.Render("⚠ ") + title
	}
	s.WriteString(title + "\n" + rule() + "\n\n")
	for _, l := range wrapLines(c.lines, panelWidth) {
		s.WriteString(l + "\n")
	}
	s.WriteString("\n" + rule() + "\n")
	if c.danger {
		s.WriteString(sBad.Render("Y") + sDim.Render(" — выполнить      ") + keyHint("n", "отмена"))
	} else {
		s.WriteString(keyHint("y/enter", "да") + "   " + keyHint("n", "отмена"))
	}
	return s.String()
}

// ───────────────────────────── экран «Колонка» ─────────────────────────────

const (
	sHost = iota
	sPort
	sUser
	sPass
	sSave
	sPC
	sCheck
	sInstall
	sRemove
	sCfg
	sDiag
	sBack
	sCount
)

func (m *model) sshCfg() SSHConfig {
	return SSHConfig{Host: m.set.SSHHost, Port: m.set.SSHPort, User: m.set.SSHUser, Password: m.set.SSHPass}
}

func (m *model) openStation() {
	m.scr = scStation
	if m.set.SSHHost == "" {
		m.stCur = sHost
	} else if m.stCur == sHost && m.status == nil {
		m.stCur = sCheck
	}
}

func (m *model) requireHost() bool {
	if strings.TrimSpace(m.set.SSHHost) == "" {
		m.stCur = sHost
		m.scr = scStation
		m.flash("сначала укажите IP колонки (Enter на «Адрес колонки»)", true)
		return false
	}
	return true
}

func validHost(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("введите IP колонки, например 192.168.3.37")
	}
	if strings.ContainsAny(s, " \t/\\@'\"`$;&|<>") {
		return errors.New("в адресе недопустимые символы")
	}
	return nil
}

func (m *model) startEdit(item int) {
	switch item {
	case sHost:
		m.stEdit = newField(m.set.SSHHost, false, false, 64)
	case sPort:
		m.stEdit = newField(strconv.Itoa(m.set.SSHPort), false, true, 5)
	case sUser:
		m.stEdit = newField(m.set.SSHUser, false, false, 32)
	case sPass:
		m.stEdit = newField(m.set.SSHPass, true, false, 128)
	case sPC:
		m.stEdit = newField(m.set.PCIP, false, false, 45)
	}
}

func (m *model) commitEdit(item int) {
	val := m.stEdit.String()
	m.stEdit = nil
	switch item {
	case sHost:
		if err := validHost(val); err != nil {
			m.flash(err.Error(), true)
			return
		}
		m.set.SSHHost = strings.TrimSpace(val)
	case sPort:
		p, err := strconv.Atoi(val)
		if err != nil || p < 1 || p > 65535 {
			m.flash("порт должен быть числом от 1 до 65535", true)
			return
		}
		m.set.SSHPort = p
	case sUser:
		if strings.TrimSpace(val) == "" {
			m.flash("логин не может быть пустым", true)
			return
		}
		m.set.SSHUser = strings.TrimSpace(val)
	case sPass:
		m.set.SSHPass = val
	case sPC:
		val = strings.TrimSpace(val)
		if val != "" && net.ParseIP(val) == nil {
			m.flash("это не IP-адрес. Оставьте пустым для автоопределения", true)
			return
		}
		m.set.PCIP = val
	}
	m.status, m.stErr = nil, nil
	m.refreshAutoIP()
	m.save()
}

func (m *model) keyStation(k tea.KeyMsg) tea.Cmd {
	if m.stEdit != nil {
		switch k.String() {
		case "enter":
			m.commitEdit(m.stCur)
		case "esc":
			m.stEdit = nil
		default:
			m.stEdit.handle(k)
		}
		return nil
	}
	key := normKey(k)
	switch key {
	case "esc", "q":
		m.scr = scMain
	case "up", "k", "w":
		m.stCur = (m.stCur + sCount - 1) % sCount
	case "down", "j", "s", "tab":
		m.stCur = (m.stCur + 1) % sCount
	case "c":
		return m.checkCmd()
	case "left", "right", "h", "l", "a", "d":
		if m.stCur == sSave {
			m.toggleSave()
		}
	case "enter", " ":
		switch m.stCur {
		case sHost, sPort, sUser, sPass, sPC:
			if key == "enter" {
				m.startEdit(m.stCur)
			}
		case sSave:
			m.toggleSave()
		case sCheck:
			return m.checkCmd()
		case sInstall:
			return m.askInstall()
		case sRemove:
			return m.askUninstall()
		case sCfg:
			m.scr = scCfg
		case sDiag:
			return m.openDiag(scStation)
		case sBack:
			m.scr = scMain
		}
	}
	return nil
}

func (m *model) toggleSave() {
	m.set.SavePass = !m.set.SavePass
	m.save()
	if m.set.SavePass {
		m.flash("пароль будет храниться в alisa-mic.json открытым текстом", false)
	} else {
		m.flash("пароль больше не сохраняется (в файле уже стёрт)", false)
	}
}

// checkCmd — опрос колонки (только чтение).
func (m *model) checkCmd() tea.Cmd {
	if !m.requireHost() {
		return nil
	}
	if m.checking {
		return nil
	}
	m.checking = true
	m.stErr = nil
	cfg := m.sshCfg()
	return func() tea.Msg {
		st, r, err := FetchStatus(cfg)
		if r != nil {
			r.Close()
		}
		return statusMsg{st, err}
	}
}

func (m *model) askInstall() tea.Cmd {
	if !m.requireHost() {
		return nil
	}
	ip := m.pcIP()
	if net.ParseIP(ip) == nil {
		m.stCur = sPC
		m.flash("не удалось определить IP этого ПК — впишите его вручную в «IP этого ПК»", true)
		return nil
	}
	cfg, port := m.sshCfg(), m.set.Port
	lines := []string{
		fmt.Sprintf("На колонку %s будет загружен libvqe_tap.so и изменён launch.json: звук пойдёт на %s:%d.", cfg.Host, ip, port),
		"Перед правкой автоматически создаётся бэкап исходного конфига launch.json (на колонке и на ПК).",
		"Потом колонку нужно перезагрузить.",
	}
	m.conf = &confirmState{title: "Установить / обновить скрипт", lines: lines, danger: true, yes: func() tea.Cmd {
		return m.startOp("Установка скрипта", func(l *opLog) opResult {
			if err := InstallOnStation(cfg, ip, port, l); err != nil {
				return opResult{err: err, after: func(m *model) tea.Cmd { return m.checkCmd() }}
			}
			return opResult{ok: "Скрипт установлен, бэкап сохранён. Чтобы звук пошёл, перезагрузите колонку.",
				after: func(m *model) tea.Cmd { return m.checkCmd() },
				next:  func(m *model) tea.Cmd { return m.askReboot() }}
		})
	}}
	return nil
}

func (m *model) askUninstall() tea.Cmd {
	if !m.requireHost() {
		return nil
	}
	cfg := m.sshCfg()
	m.conf = &confirmState{title: "Удалить скрипт с колонки", danger: true,
		lines: []string{
			"Конфигурация launch.json вернётся из бэкапа в исходное состояние.",
			"Файлы AlisaMic на колонке будут удалены. Алиса продолжит работать как обычно.",
			"Потом колонку нужно перезагрузить.",
		},
		yes: func() tea.Cmd {
			return m.startOp("Удаление скрипта", func(l *opLog) opResult {
				if err := UninstallFromStation(cfg, l); err != nil {
					return opResult{err: err, after: func(m *model) tea.Cmd { return m.checkCmd() }}
				}
				return opResult{ok: "Скрипт удалён, конфиг успешно возвращён из бэкапа. Перезагрузите колонку.",
					after: func(m *model) tea.Cmd { return m.checkCmd() },
					next:  func(m *model) tea.Cmd { return m.askReboot() }}
			})
		}}
	return nil
}

func (m *model) askReboot() tea.Cmd {
	if !m.requireHost() {
		return nil
	}
	cfg := m.sshCfg()
	m.conf = &confirmState{title: "Перезагрузить колонку?", danger: true,
		lines: []string{"Алиса будет недоступна 1–2 минуты. Программа дождётся, пока SSH снова ответит."},
		yes: func() tea.Cmd {
			return m.startOp("Перезагрузка колонки", func(l *opLog) opResult {
				if err := RebootStation(cfg, l, true); err != nil {
					return opResult{err: err}
				}
				return opResult{ok: "Колонка перезагружена.", after: func(m *model) tea.Cmd { return m.checkCmd() }}
			})
		}}
	return nil
}

func (m *model) statusLines() []string {
	var out []string
	row := func(mark, label, text string) {
		out = append(out, mark+" "+sDim.Render(padRight(label, 11))+truncate(text, panelWidth-14))
	}
	ok, warn, bad, dim := sOK.Render("✓"), sWarn.Render("!"), sBad.Render("✕"), sDim.Render("–")
	switch {
	case m.checking:
		row(sBlue.Render("◌"), "Связь", "проверяю…")
		return append(out, "", "", "", "")
	case m.stErr != nil:
		row(bad, "Связь", m.stErr.Error())
		return append(out, "", "", "", "")
	case m.status == nil:
		row(dim, "Связь", "не проверялась — «Проверить колонку» (C)")
		return append(out, "", "", "", "")
	}
	st := m.status
	row(ok, "Связь", fmt.Sprintf("%s@%s · %s · аптайм %s", m.set.SSHUser, m.set.SSHHost, st.s("arch"), fmtUptime(st.n("uptime"))))
	switch {
	case !st.b("vqe_symbol"):
		row(bad, "Прошивка", "VQE не найден — прошивка не поддерживается")
	default:
		row(ok, "Прошивка", "VQE найден — совместима")
	}
	switch {
	case !st.b("so"):
		row(bad, "Скрипт", "не установлен")
	case !st.b("patched"):
		row(bad, "Скрипт", "файл есть, но launch.json не пропатчен")
	case !st.UpToDate():
		row(warn, "Скрипт", "установлен, версия устарела — обновите")
	default:
		row(ok, "Скрипт", "установлен, версия актуальна")
	}
	want := net.JoinHostPort(m.pcIP(), strconv.Itoa(m.set.Port))
	switch {
	case !st.b("patched"):
		row(dim, "Приёмник", "—")
	case st.s("target") == want:
		row(ok, "Приёмник", st.s("target")+" (этот ПК)")
	default:
		row(warn, "Приёмник", fmt.Sprintf("колонка шлёт на %s, а ПК: %s", orDash(st.s("target")), want))
	}
	switch {
	case !st.b("patched"):
		row(dim, "В работе", "—")
	case st.b("loaded"):
		row(ok, "В работе", "libvqe_tap.so загружен в голосовой движок")
	case !st.b("maind"):
		row(warn, "В работе", "maind не найден (колонка грузится?)")
	default:
		row(warn, "В работе", "нужна перезагрузка колонки")
	}
	return out
}

func (m *model) viewStation() string {
	conn := sDim.Render("○ не проверена")
	switch {
	case m.checking:
		conn = sBlue.Render("◌ проверяю…")
	case m.stErr != nil:
		conn = sBad.Render("● нет связи")
	case m.status != nil:
		conn = sOK.Render("● на связи")
	}
	val := func(item int, s string) string {
		if m.stEdit != nil && m.stCur == item {
			return m.stEdit.view()
		}
		return s
	}
	pass := "не задан"
	if m.set.SSHPass != "" {
		pass = strings.Repeat("•", len([]rune(m.set.SSHPass)))
	}
	pc := m.autoIP + " (авто)"
	if m.set.PCIP != "" {
		pc = m.set.PCIP
	}
	host := m.set.SSHHost
	if host == "" {
		host = "не указан"
	}
	rows := make([]lrow, sCount)
	rows[sHost] = lrow{label: "Адрес колонки", value: val(sHost, host)}
	rows[sPort] = lrow{label: "SSH-порт", value: val(sPort, strconv.Itoa(m.set.SSHPort))}
	rows[sUser] = lrow{label: "Пользователь", value: val(sUser, m.set.SSHUser)}
	rows[sPass] = lrow{label: "Пароль", value: val(sPass, pass)}
	rows[sSave] = lrow{label: "Запомнить пароль", value: onOff(m.set.SavePass)}
	rows[sPC] = lrow{label: "IP этого ПК", value: val(sPC, pc), gap: true}
	rows[sCheck] = lrow{label: "▶ Проверить колонку"}
	rows[sInstall] = lrow{label: "⬇ Установить / обновить скрипт"}
	rows[sRemove] = lrow{label: "✕ Удалить скрипт с колонки"}
	rows[sCfg] = lrow{label: "⚙ Настройки колонки (SSH, пароль)…"}
	rows[sDiag] = lrow{label: "✚ Диагностика…"}
	rows[sBack] = lrow{label: "← Назад"}

	var b strings.Builder
	b.WriteString(m.headerWith("Колонка · SSH", conn) + "\n" + rule() + "\n\n")
	b.WriteString(renderRows(rows, m.stCur, 18))
	b.WriteString("\n" + rule() + "\n")
	for _, l := range m.statusLines() {
		b.WriteString(l + "\n")
	}
	b.WriteString(rule() + "\n")
	b.WriteString(m.msgOrHint(m.stationHint()) + "\n\n")
	if m.stEdit != nil {
		b.WriteString(keyHint("enter", "сохранить") + "   " + keyHint("esc", "отмена") + "   " + keyHint("ctrl+u", "очистить"))
	} else {
		b.WriteString(keyHint("↑↓", "выбор") + "   " + keyHint("enter", "изменить/запустить") + "   " + keyHint("c", "проверить") + "   " + keyHint("esc", "назад"))
	}
	return b.String()
}

func (m *model) stationHint() string {
	switch m.stCur {
	case sHost:
		return "IP колонки в вашей сети (список клиентов в роутере)"
	case sPort:
		return "У рутованных станций обычно 2222"
	case sUser:
		return "Обычно root"
	case sPass:
		return "Пароль SSH колонки (может быть пустым)"
	case sSave:
		return "Хранить пароль в alisa-mic.json открытым текстом"
	case sPC:
		return "Куда колонка шлёт звук. Обычно определяется само"
	case sCheck:
		return "Только чтение: подключиться и посмотреть, что установлено"
	case sInstall:
		return "Положить libvqe_tap.so и пропатчить launch.json"
	case sRemove:
		return "Откатить launch.json из бэкапа и убрать libvqe_tap.so"
	case sCfg:
		return "Сменить SSH-порт и пароль, перезагрузка, логи"
	case sDiag:
		return "Проверить всё сразу и предложить исправления"
	}
	return "Адрес и пароль сохраняются автоматически"
}

// ───────────────────────────── «Настройки колонки» ─────────────────────────────

const (
	cPort = iota
	cPass
	cRetarget
	cReboot
	cInfo
	cLogs
	cForget
	cBack
	cCount
)

func (m *model) keyCfg(k tea.KeyMsg) tea.Cmd {
	switch normKey(k) {
	case "esc", "q":
		m.scr = scStation
	case "up", "k", "w":
		m.cfgCur = (m.cfgCur + cCount - 1) % cCount
	case "down", "j", "s", "tab":
		m.cfgCur = (m.cfgCur + 1) % cCount
	case "enter", " ":
		return m.activateCfg()
	}
	return nil
}

func (m *model) activateCfg() tea.Cmd {
	if m.cfgCur != cBack && m.cfgCur != cForget && !m.requireHost() {
		return nil
	}
	switch m.cfgCur {
	case cPort:
		m.prompt = &promptState{
			title: "Новый SSH-порт колонки", label: "Порт, на котором колонка будет принимать SSH:",
			hint: "Сначала поднимется второй SSH-сервер на новом порту, вход проверится, и только потом закроется старый. Если проверка не пройдёт — всё откатится.",
			f:    newField("", false, true, 5),
			validate: func(s string) error {
				p, err := strconv.Atoi(s)
				if err != nil || p < 1 || p > 65535 {
					return errors.New("порт должен быть числом от 1 до 65535")
				}
				if p == m.set.SSHPort {
					return errors.New("это текущий порт")
				}
				if p == 5555 || p == 1961 || p == 80 || p == 443 {
					return errors.New("порт занят службами колонки, выберите другой")
				}
				return nil
			},
			submit: func(s string) tea.Cmd { p, _ := strconv.Atoi(s); return m.askChangePort(p) },
		}
	case cPass:
		m.prompt = &promptState{
			title: "Новый пароль SSH колонки", label: "Пароль для пользователя root на колонке:",
			hint: "Колонка получит только хеш пароля. Старый файл с паролями сохранится как *.bak-alisamic.",
			f:    newField("", true, false, 128),
			validate: func(s string) error {
				if s == "" {
					return errors.New("введите пароль")
				}
				return nil
			},
			submit: func(first string) tea.Cmd {
				m.prompt = &promptState{
					title: "Повторите пароль", label: "Ещё раз тот же пароль:",
					f: newField("", true, false, 128),
					validate: func(s string) error {
						if s != first {
							return errors.New("пароли не совпадают")
						}
						return nil
					},
					submit: func(s string) tea.Cmd { return m.askChangePass(s) },
				}
				return nil
			},
		}
	case cRetarget:
		return m.askInstall()
	case cReboot:
		return m.askReboot()
	case cInfo:
		return m.textOp("Информация о колонке", StationInfo)
	case cLogs:
		return m.textOp("Логи колонки", StationLogs)
	case cForget:
		if m.set.SSHHost == "" {
			m.flash("IP колонки не указан", true)
			return nil
		}
		a := m.sshCfg().addr()
		if knownHostKey(a) == "" {
			m.flash("для "+a+" ключ ещё не запоминался", false)
			return nil
		}
		rememberHostKey(a, "")
		m.flash("ключ колонки забыт — при следующем подключении запомню новый", false)
	case cBack:
		m.scr = scStation
	}
	return nil
}

func (m *model) askChangePort(newPort int) tea.Cmd {
	cfg := m.sshCfg()
	m.conf = &confirmState{title: "Сменить SSH-порт колонки", danger: true,
		lines: []string{
			fmt.Sprintf("Порт SSH станет %d вместо %d. Правится init-скрипт dropbear (с бэкапом).", newPort, cfg.Port),
			"Если что-то пойдёт не так — программа откатит изменения сама. Запасной вход: ADB.",
		},
		yes: func() tea.Cmd {
			return m.startOp("Смена SSH-порта", func(l *opLog) opResult {
				if err := ChangeSSHPort(cfg, newPort, l); err != nil {
					return opResult{err: err}
				}
				return opResult{ok: fmt.Sprintf("Готово: SSH теперь на порту %d. Настройка в программе обновлена.", newPort),
					after: func(m *model) tea.Cmd {
						m.set.SSHPort = newPort
						m.save()
						m.status = nil
						return m.checkCmd()
					}}
			})
		}}
	return nil
}

func (m *model) askChangePass(newPass string) tea.Cmd {
	cfg := m.sshCfg()
	m.conf = &confirmState{title: "Сменить пароль SSH колонки", danger: true,
		lines: []string{
			"Будет изменён файл паролей на колонке (с бэкапом). Программа проверит вход с новым паролем и при неудаче вернёт старый.",
			"Запомните новый пароль: без него в колонку по SSH не войти (останется только ADB).",
		},
		yes: func() tea.Cmd {
			return m.startOp("Смена пароля SSH", func(l *opLog) opResult {
				if err := ChangeSSHPassword(cfg, newPass, l); err != nil {
					return opResult{err: err}
				}
				return opResult{ok: "Пароль изменён, вход с новым паролем проверен.",
					after: func(m *model) tea.Cmd {
						m.set.SSHPass = newPass
						m.save()
						return nil
					}}
			})
		}}
	return nil
}

// textOp выполняет запрос к колонке и показывает результат в окне просмотра.
func (m *model) textOp(title string, fn func(SSHConfig) ([]string, error)) tea.Cmd {
	cfg := m.sshCfg()
	return m.startOp(title, func(l *opLog) opResult {
		l.add("читаю с колонки…")
		lines, err := fn(cfg)
		if err != nil {
			return opResult{err: err}
		}
		return opResult{silent: true, after: func(m *model) tea.Cmd {
			m.txtTitle, m.txtLines, m.txtScroll = title, lines, 0
			return nil
		}, next: func(m *model) tea.Cmd { m.prev, m.scr = scCfg, scText; return nil }}
	})
}

func (m *model) viewCfg() string {
	rows := make([]lrow, cCount)
	rows[cPort] = lrow{label: "Сменить SSH-порт", value: strconv.Itoa(m.set.SSHPort)}
	rows[cPass] = lrow{label: "Сменить пароль SSH"}
	rows[cRetarget] = lrow{label: "Направить звук на этот ПК", value: m.pcIP() + ":" + strconv.Itoa(m.set.Port)}
	rows[cReboot] = lrow{label: "Перезагрузить колонку"}
	rows[cInfo] = lrow{label: "Информация о колонке"}
	rows[cLogs] = lrow{label: "Логи колонки"}
	rows[cForget] = lrow{label: "Забыть ключ SSH колонки"}
	rows[cBack] = lrow{label: "← Назад"}

	var b strings.Builder
	conn := sDim.Render(m.set.SSHHost)
	b.WriteString(m.headerWith("Настройки колонки", conn) + "\n" + rule() + "\n\n")
	b.WriteString(renderRows(rows, m.cfgCur, 28))
	b.WriteString("\n" + rule() + "\n")
	warn := []string{
		"Все изменения делаются по SSH с правами root, перед правкой",
		"создаются бэкапы *.bak-alisamic. Запасной вход в колонку — ADB.",
	}
	for _, l := range warn {
		b.WriteString(sWarn.Render(l) + "\n")
	}
	b.WriteString(rule() + "\n")
	b.WriteString(m.msgOrHint(m.cfgHint()) + "\n\n")
	b.WriteString(keyHint("↑↓", "выбор") + "   " + keyHint("enter", "выполнить") + "   " + keyHint("esc", "назад"))
	return b.String()
}

func (m *model) cfgHint() string {
	switch m.cfgCur {
	case cPort:
		return "Безопасно: новый порт проверяется до закрытия старого"
	case cPass:
		return "Меняет пароль root для входа по SSH"
	case cRetarget:
		return "Если у ПК сменился IP — колонка должна знать новый"
	case cReboot:
		return "Нужна после установки скрипта или смены адреса"
	case cInfo:
		return "Версия системы, сеть, звуковые карты, состояние скрипта"
	case cLogs:
		return "dmesg, logread и лог SSH-сервера (хвост)"
	case cForget:
		return "Если ключ колонки изменился после сброса или переустановки"
	}
	return ""
}

// ───────────────────────────── просмотр текста ─────────────────────────────

const textRows = 18

func (m *model) keyText(k tea.KeyMsg) tea.Cmd {
	maxScroll := len(m.txtLines) - textRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	switch normKey(k) {
	case "esc", "q", "enter":
		m.scr = m.prev
		if m.scr == scSplash || m.scr == scText {
			m.scr = scMain
		}
	case "up", "k", "w":
		m.txtScroll--
	case "down", "j", "s":
		m.txtScroll++
	case "pgup":
		m.txtScroll -= textRows - 2
	case "pgdown", " ":
		m.txtScroll += textRows - 2
	case "home":
		m.txtScroll = 0
	case "end":
		m.txtScroll = maxScroll
	}
	m.txtScroll = int(clampF(float64(m.txtScroll), 0, float64(maxScroll)))
	return nil
}

func (m *model) viewText() string {
	var b strings.Builder
	b.WriteString(gradientText(m.txtTitle) + "\n" + rule() + "\n")
	lines := m.txtLines
	end := m.txtScroll + textRows
	if end > len(lines) {
		end = len(lines)
	}
	shown := 0
	for _, l := range lines[m.txtScroll:end] {
		b.WriteString(sDim.Render(truncate(strings.ReplaceAll(l, "\t", "  "), panelWidth)) + "\n")
		shown++
	}
	for ; shown < textRows; shown++ {
		b.WriteString("\n")
	}
	b.WriteString(rule() + "\n")
	pos := fmt.Sprintf("%d–%d из %d", m.txtScroll+1, end, len(lines))
	b.WriteString(sDim.Render(pos) + "   " + keyHint("↑↓", "листать") + "   " + keyHint("pgup/pgdn", "страница") + "   " + keyHint("esc", "назад"))
	return b.String()
}
