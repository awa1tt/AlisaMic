package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	scSplash screen = iota
	scDisclaimer
	scMain
	scMonitor
	scStation
	scCfg
	scDiag
	scText
)

type item int

const (
	itStream item = iota
	itOutput
	itAGC
	itGain
	itPort
	itBuffer
	itAutostart
	itMonitor
	itStation
	itDiag
	itAbout
	itQuit
	itCount
)

const (
	panelWidth = 62
	minWidth   = panelWidth
	minHeight  = 24
)

type tickMsg time.Time
type splashDoneMsg struct{}
type startResultMsg struct{ err error }

type model struct {
	scr, prev screen
	w, h      int
	cursor    item

	set    Settings
	outs   []Device
	outIdx int
	ins    []Device
	inIdx  int
	engine *Engine
	autoIP string // IP этого ПК, определённый автоматически

	artRows []string
	artW    int

	edit     *field // редактор порта на главном экране
	starting bool

	msg     string
	msgErr  bool
	msgTill time.Time

	// сглаженные уровни и скорость пакетов
	inLvl, outLvl float64
	pps           float64
	lastPkts      uint64
	lastRate      time.Time

	// монитор устройства записи
	rec      *Recorder
	monRMS   atomicFloat
	monPeak  atomic.Int64
	monShown float64
	monErr   string

	// колонка
	stCur    int
	stEdit   *field
	status   *StationStatus
	stErr    error
	checking     bool
	checkStarted time.Time
	cfgCur       int

	// просмотр текста (информация, логи)
	txtTitle  string
	txtLines  []string
	txtScroll int

	// диагностика
	diag     []Check
	diagCur  int
	diagBusy bool
	diagWhen time.Time

	// модальные окна
	busy   *busyState
	res    *resultState
	prompt *promptState
	conf   *confirmState
	opID   int

	disclaimerRO bool // окно отказа открыто из меню — только чтение
}

func newModel(s Settings, e *Engine) *model {
	m := &model{scr: scSplash, set: s, engine: e, lastRate: time.Now()}
	m.refreshAutoIP()
	m.artRows, m.artW = renderArt()
	m.reloadDevices()
	if i := FindDevice(m.outs, s.Output); i >= 0 {
		m.outIdx = i
	} else if s.Output != "" {
		m.flash(fmt.Sprintf("«%s» не найдено — выбраны колонки по умолчанию", s.Output), true)
	}
	return m
}

func (m *model) refreshAutoIP() { m.autoIP = localIPFor(m.set.SSHHost) }

// pcIP — IP этого ПК, на который колонка должна слать звук.
func (m *model) pcIP() string {
	switch {
	case m.set.PCIP != "":
		return m.set.PCIP
	case m.status != nil && m.status.LocalIP != "":
		return m.status.LocalIP
	}
	return m.autoIP
}

func (m *model) reloadDevices() {
	cur := ""
	if m.outIdx < len(m.outs) {
		cur = m.outs[m.outIdx].Name
	}
	m.outs = OutputDevices()
	if i := FindDevice(m.outs, cur); i >= 0 {
		m.outIdx = i
	} else {
		m.outIdx = 0
	}
	curIn := ""
	if m.inIdx < len(m.ins) {
		curIn = m.ins[m.inIdx].Name
	}
	m.ins = InputDevices()
	m.inIdx = 0
	if i := FindDevice(m.ins, curIn); curIn != "" && i >= 0 {
		m.inIdx = i
	} else if i := FindDevice(m.ins, "CABLE Output"); i >= 0 {
		m.inIdx = i
	}
}

func (m *model) flash(s string, isErr bool) {
	m.msg, m.msgErr, m.msgTill = s, isErr, time.Now().Add(6*time.Second)
}

func tick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("AlisaMic"), tick(),
		tea.Tick(2600*time.Millisecond, func(time.Time) tea.Msg { return splashDoneMsg{} })}
	return tea.Batch(cmds...)
}

// afterSplash — первый запуск показывает отказ от ответственности, дальше сразу меню.
func (m *model) afterSplash() tea.Cmd {
	if m.set.Accepted != disclaimerVersion {
		m.scr = scDisclaimer
		return nil
	}
	m.scr = scMain
	return m.autostart()
}

func (m *model) autostart() tea.Cmd {
	if m.set.Autostart {
		return m.startCmd()
	}
	return nil
}

// startCmd запускает приём звука. Устройство ищется по имени в момент запуска —
// номера устройств Windows могут поменяться, пока открыто меню.
func (m *model) startCmd() tea.Cmd {
	if m.starting || m.engine.Running() {
		return nil
	}
	m.starting = true
	s, name := m.set, m.set.Output
	return func() tea.Msg { return startResultMsg{startEngine(m.engine, s, name)} }
}

func startEngine(e *Engine, s Settings, name string) error {
	outs := OutputDevices()
	dev := outs[0]
	if name != "" {
		i := FindDevice(outs, name)
		if i < 0 {
			return fmt.Errorf("устройство «%s» не найдено (R — обновить список)", name)
		}
		dev = outs[i]
	}
	return e.Start(s, dev)
}

// restartIfRunning — применяет настройки, требующие переоткрыть порт/устройство.
func (m *model) restartIfRunning() tea.Cmd {
	if !m.engine.Running() || m.starting {
		return nil
	}
	m.starting = true
	s, name, e := m.set, m.set.Output, m.engine
	return func() tea.Msg {
		e.Stop()
		return startResultMsg{startEngine(e, s, name)}
	}
}

func (m *model) save() {
	if err := m.set.save(); err != nil {
		m.flash("не удалось сохранить настройки: "+err.Error(), true)
	}
}

// ───────────────────────────── обновление ─────────────────────────────

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case splashDoneMsg:
		if m.scr == scSplash {
			return m, m.afterSplash()
		}
		return m, nil

	case startResultMsg:
		m.starting = false
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
		}
		return m, nil

	case tickMsg:
		m.onTick()
		return m, tick()

	case opDoneMsg:
		return m, m.onOpDone(msg)

	case statusMsg:
		m.checking = false
		m.status, m.stErr = msg.st, msg.err
		return m, nil

	case diagMsg:
		m.diagBusy = false
		m.diag, m.diagWhen = msg.checks, time.Now()
		if msg.st != nil {
			m.status, m.stErr = msg.st, nil
		}
		if m.diagCur >= len(m.diag) {
			m.diagCur = 0
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)
	}
	return m, nil
}

func (m *model) onTick() {
	if m.msg != "" && time.Now().After(m.msgTill) {
		m.msg = ""
	}
	// плавное падение стрелок, быстрый подъём
	decay := func(shown, v float64) float64 {
		if v > shown {
			return v
		}
		return shown*0.82 + v*0.18
	}
	in, out := 0.0, 0.0
	if m.engine.Running() && m.engine.HasSignal() {
		in, out = m.engine.InLevel.Load(), m.engine.OutLevel.Load()
	}
	m.inLvl, m.outLvl = decay(m.inLvl, in), decay(m.outLvl, out)
	m.monShown = decay(m.monShown, m.monRMS.Load())

	if dt := time.Since(m.lastRate); dt >= time.Second {
		p := m.engine.Packets.Load()
		if p < m.lastPkts {
			m.lastPkts = 0
		}
		m.pps = float64(p-m.lastPkts) / dt.Seconds()
		m.lastPkts, m.lastRate = p, time.Now()
	}
}

func (m *model) quit() tea.Cmd {
	m.stopMonitor()
	m.engine.Stop()
	return tea.Quit
}

// askQuit — если идёт трансляция, спрашиваем, чтобы случайный Esc не оборвал микрофон.
func (m *model) askQuit() tea.Cmd {
	if !m.engine.Running() {
		return m.quit()
	}
	m.conf = &confirmState{
		title: "Выйти из AlisaMic?",
		lines: []string{"Трансляция микрофона идёт — после выхода звук пропадёт."},
		yes:   func() tea.Cmd { return m.quit() },
	}
	return nil
}

func (m *model) onKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "ctrl+c" {
		return m.quit()
	}
	switch {
	case m.busy != nil:
		return nil
	case m.res != nil:
		return m.keyResult(k)
	case m.prompt != nil:
		return m.keyPrompt(k)
	case m.conf != nil:
		return m.keyConfirm(k)
	}
	switch m.scr {
	case scSplash:
		return m.afterSplash()
	case scDisclaimer:
		return m.keyDisclaimer(k)
	case scMonitor:
		return m.keyMonitor(k)
	case scStation:
		return m.keyStation(k)
	case scCfg:
		return m.keyCfg(k)
	case scDiag:
		return m.keyDiag(k)
	case scText:
		return m.keyText(k)
	}
	return m.keyMain(k)
}

func (m *model) keyDisclaimer(k tea.KeyMsg) tea.Cmd {
	key := normKey(k)
	if m.disclaimerRO {
		switch key {
		case "esc", "enter", "q", " ":
			m.scr, m.disclaimerRO = scMain, false
		}
		return nil
	}
	switch key {
	case "y":
		m.set.Accepted = disclaimerVersion
		m.save()
		m.scr = scMain
		return m.autostart()
	case "q", "esc", "n":
		return m.quit()
	}
	return nil
}

func (m *model) keyMain(k tea.KeyMsg) tea.Cmd {
	if m.edit != nil {
		switch k.String() {
		case "enter":
			p, err := strconv.Atoi(m.edit.String())
			m.edit = nil
			if err != nil || p < 1 || p > 65535 {
				m.flash("порт должен быть числом от 1 до 65535", true)
				return nil
			}
			if p != m.set.Port {
				m.set.Port = p
				m.save()
				m.flash("порт изменён — не забудьте обновить адрес на колонке (меню «Колонка»)", false)
				return m.restartIfRunning()
			}
		case "esc":
			m.edit = nil
		default:
			m.edit.handle(k)
		}
		return nil
	}

	switch key := normKey(k); key {
	case "q", "esc":
		return m.askQuit()
	case "up", "k", "w":
		m.cursor = (m.cursor + itCount - 1) % itCount
	case "down", "j", "s", "tab":
		m.cursor = (m.cursor + 1) % itCount
	case "r":
		m.reloadDevices()
		m.flash("список устройств обновлён", false)
	case " ":
		if m.cursor == itStream {
			return m.toggleStream()
		}
		return m.change(+1)
	case "left", "h", "a":
		return m.change(-1)
	case "right", "l", "d":
		return m.change(+1)
	case "enter":
		switch m.cursor {
		case itStream:
			return m.toggleStream()
		case itPort:
			m.edit = newField("", false, true, 5)
		case itMonitor:
			m.openMonitor()
		case itStation:
			m.openStation()
		case itDiag:
			return m.openDiag(scMain)
		case itAbout:
			m.scr, m.disclaimerRO = scDisclaimer, true
		case itQuit:
			return m.askQuit()
		default:
			return m.change(+1)
		}
	}
	return nil
}

func (m *model) toggleStream() tea.Cmd {
	if m.starting {
		return nil
	}
	if m.engine.Running() {
		m.engine.Stop()
		return nil
	}
	m.stopMonitor()
	return m.startCmd()
}

func gainStep(g float64, dir int) float64 {
	step := 10.0
	switch {
	case g < 10 || (g == 10 && dir < 0):
		step = 1
	case g < 50 || (g == 50 && dir < 0):
		step = 5
	}
	return clampF(g+step*float64(dir), 1, 200)
}

func (m *model) change(dir int) tea.Cmd {
	switch m.cursor {
	case itStream:
		if (dir > 0) != m.engine.Running() {
			return m.toggleStream()
		}
	case itOutput:
		m.outIdx = (m.outIdx + len(m.outs) + dir) % len(m.outs)
		m.set.Output = m.outs[m.outIdx].Name
		if m.outs[m.outIdx].IsDefault() {
			m.set.Output = ""
		}
		m.save()
		return m.restartIfRunning()
	case itAGC:
		m.set.AGC = !m.set.AGC
		m.engine.SetAGC(m.set.AGC)
		if !m.set.AGC && m.set.Gain > 20 {
			m.flash("автогейн выключен — усиление теперь постоянное, проверьте громкость", false)
		}
		m.save()
	case itGain:
		m.set.Gain = gainStep(m.set.Gain, dir)
		m.engine.SetGain(m.set.Gain)
		m.save()
	case itBuffer:
		m.set.BufferMs = int(clampF(float64(m.set.BufferMs+dir*chunkMs), 120, 1000))
		m.save()
		return m.restartIfRunning()
	case itAutostart:
		m.set.Autostart = !m.set.Autostart
		m.save()
	case itPort:
		m.edit = newField("", false, true, 5)
	}
	return nil
}

// ───────────────────────────── монитор ─────────────────────────────

func (m *model) openMonitor() {
	m.reloadDevices()
	m.scr = scMonitor
	m.startMonitor()
}

func (m *model) startMonitor() {
	m.stopMonitor()
	m.monErr = ""
	if len(m.ins) == 0 {
		m.monErr = "в системе нет устройств записи"
		return
	}
	rec, err := StartRecorder(m.ins[m.inIdx].ID, func(pcm []byte) {
		rms, peak := rmsOf(pcm)
		m.monRMS.Store(rms)
		m.monPeak.Store(int64(peak))
	})
	if err != nil {
		m.monErr = err.Error()
		return
	}
	m.rec = rec
}

func (m *model) stopMonitor() {
	if m.rec != nil {
		m.rec.Stop()
		m.rec = nil
	}
	m.monRMS.Store(0)
	m.monPeak.Store(0)
	m.monShown = 0
}

func (m *model) keyMonitor(k tea.KeyMsg) tea.Cmd {
	switch key := normKey(k); key {
	case "esc", "q", "enter", "backspace":
		m.stopMonitor()
		m.scr = scMain
	case "left", "h", "a", "right", "l", "d":
		if len(m.ins) > 0 {
			dir := 1
			if key == "left" || key == "h" || key == "a" {
				dir = -1
			}
			m.inIdx = (m.inIdx + len(m.ins) + dir) % len(m.ins)
			m.startMonitor()
		}
	case "r":
		m.reloadDevices()
		m.startMonitor()
	}
	return nil
}

// ───────────────────────────── отрисовка ─────────────────────────────

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	var body string
	switch {
	case m.scr == scSplash:
		return m.viewSplash()
	case m.busy != nil:
		body = m.viewBusy()
	case m.res != nil:
		body = m.viewResult()
	case m.prompt != nil:
		body = m.viewPrompt()
	case m.conf != nil:
		body = m.viewConfirm()
	default:
		switch m.scr {
		case scDisclaimer:
			body = m.viewDisclaimer()
		case scMonitor:
			body = m.viewMonitor()
		case scStation:
			body = m.viewStation()
		case scCfg:
			body = m.viewCfg()
		case scDiag:
			body = m.viewDiag()
		case scText:
			body = m.viewText()
		default:
			body = m.viewMain()
		}
	}
	bh := lipgloss.Height(body)
	if m.w < minWidth || m.h < minHeight {
		return m.viewTooSmall(bh)
	}
	top := 0
	if extra := m.h - bh; extra > 0 {
		top = extra / 3
		if top > 1 {
			top = 1
		}
	}
	left := (m.w - lipgloss.Width(body)) / 2
	if left < 0 {
		left = 0
	}
	return strings.Repeat("\n", top) + lipgloss.NewStyle().MarginLeft(left).Render(body)
}

func (m *model) viewTooSmall(needH int) string {
	return "\n " + sPurple.Render("Окно консоли слишком маленькое") + "\n\n " +
		sDim.Render(fmt.Sprintf("Текущий размер: %d×%d (требуется от %d×%d).", m.w, m.h, minWidth, minHeight)) + "\n " +
		sDim.Render("Пожалуйста, растяните окно консоли.") + "\n\n " +
		sDim.Render("Ctrl+C — выход")
}

func (m *model) viewSplash() string {
	rows := m.artRows
	room := m.h - 5
	if room < len(rows) { // окно низкое — обрезаем арт поровну сверху и снизу
		if room < 0 {
			room = 0
		}
		cut := (len(rows) - room) / 2
		rows = rows[cut : cut+room]
	}
	if m.w < m.artW {
		rows = nil
	}
	parts := append([]string{}, rows...)
	parts = append(parts, "", gradientText("AlisaMic")+"  "+sDim.Render(version))
	if m.h > len(rows)+4 {
		parts = append(parts, sDim.Render("Алиса · микрофон для ПК"))
	}
	parts = append(parts, sBlue.Render(repoURL))
	block := lipgloss.JoinVertical(lipgloss.Center, parts...)
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, block)
}

func rule() string { return sDim.Render(strings.Repeat("─", panelWidth)) }

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func (m *model) headerWith(title, status string) string {
	left := gradientText(title)
	gap := panelWidth - lipgloss.Width(left) - lipgloss.Width(status)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + status
}

func (m *model) streamStatus() string {
	switch {
	case m.starting:
		return sBlue.Render("◌ запуск…")
	case !m.engine.Running():
		return sDim.Render("○ остановлено")
	case m.engine.HasSignal():
		return sPurple.Render("● в эфире")
	}
	return sBlue.Render("● ждём колонку")
}

func (m *model) header(title string) string { return m.headerWith(title, m.streamStatus()) }

func onOff(v bool) string {
	if v {
		return "вкл"
	}
	return "выкл"
}

type lrow struct {
	label, value string
	arrows       bool
	gap          bool // пустая строка после пункта
}

func renderRows(rows []lrow, cursor int, labelW int) string {
	var b strings.Builder
	for i, r := range rows {
		sel := i == cursor
		label := padRight(r.label, labelW)
		value := r.value
		if sel && r.arrows {
			value = sDim.Render("‹ ") + sValue.Render(value) + sDim.Render(" ›")
		} else if value != "" && !strings.Contains(value, "\x1b") {
			value = sValue.Render(value)
		}
		if sel {
			b.WriteString(sPurple.Render("  › ") + sSelected.Render(label) + value + "\n")
		} else {
			b.WriteString("    " + label + value + "\n")
		}
		if r.gap {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func (m *model) viewMain() string {
	running := m.engine.Running()

	gainLabel := "Предел усиления"
	if !m.set.AGC {
		gainLabel = "Усиление"
	}
	stream := "выключена"
	if running {
		stream = "включена"
	}
	port := strconv.Itoa(m.set.Port)
	if m.edit != nil {
		port = m.edit.view()
	}
	station := "не настроена"
	if m.set.SSHHost != "" {
		station = m.set.SSHHost
	}
	rows := make([]lrow, itCount)
	rows[itStream] = lrow{label: "Трансляция", value: stream}
	rows[itOutput] = lrow{label: "Вывод", value: truncate(m.outs[m.outIdx].Name, 34), arrows: true}
	rows[itAGC] = lrow{label: "Автогейн", value: onOff(m.set.AGC)}
	rows[itGain] = lrow{label: gainLabel, value: "×" + strconv.FormatFloat(m.set.Gain, 'f', -1, 64), arrows: true}
	rows[itPort] = lrow{label: "Порт UDP", value: port}
	rows[itBuffer] = lrow{label: "Буфер", value: fmt.Sprintf("%d мс", m.set.BufferMs), arrows: true}
	rows[itAutostart] = lrow{label: "Автозапуск", value: onOff(m.set.Autostart), gap: true}
	rows[itMonitor] = lrow{label: "Проверка микрофона"}
	rows[itStation] = lrow{label: "Колонка (SSH)…", value: station}
	rows[itDiag] = lrow{label: "Диагностика…"}
	rows[itAbout] = lrow{label: "О программе (About)…"}
	rows[itQuit] = lrow{label: "Выход"}

	var b strings.Builder
	b.WriteString(m.header("AlisaMic") + "\n" + rule() + "\n\n")
	b.WriteString(renderRows(rows, int(m.cursor), 24))

	b.WriteString("\n" + rule() + "\n")
	b.WriteString(m.viewStats(running))
	b.WriteString(rule() + "\n")
	b.WriteString(m.msgOrHint(m.hint()) + "\n")
	b.WriteString("\n" + m.keys() + "\n\n")
	b.WriteString("  " + sWarn.Render(testedShort))
	return b.String()
}

func (m *model) msgOrHint(hint string) string {
	if m.msg != "" {
		st, mark := sBlue, "· "
		if m.msgErr {
			st, mark = sBad, "✕ "
		}
		return st.Render(mark + truncate(m.msg, panelWidth-2))
	}
	return sDim.Render(truncate(hint, panelWidth))
}

func (m *model) viewStats(running bool) string {
	const lw = 12
	line := func(label, value string) string { return sDim.Render(padRight(label, lw)) + value + "\n" }

	src := sDim.Render("—")
	if running {
		if m.engine.HasSignal() {
			src = sValue.Render(m.engine.Sender.Load().(string)) + sDim.Render(fmt.Sprintf("  ·  %.0f пак/с", m.pps))
		} else {
			src = sBlue.Render("ждём звук на порту " + strconv.Itoa(m.set.Port) + "…")
		}
	}
	gain := ""
	if running && m.engine.HasSignal() {
		gain = sDim.Render(fmt.Sprintf("  ×%.1f", m.engine.CurGain.Load()))
	}
	queue := sDim.Render("—")
	if running {
		queue = sDim.Render(fmt.Sprintf("%d мс  ·  потерь %d", m.engine.QueuedMs.Load(), m.engine.Drops.Load()))
	}
	var b strings.Builder
	b.WriteString(line("Колонка", src))
	b.WriteString(line("Вход", meter(m.inLvl, 40, running)))
	b.WriteString(line("Выход", meter(m.outLvl, 40, running)+gain))
	b.WriteString(line("Очередь", queue))
	b.WriteString(line("Этот ПК", sDim.Render(fmt.Sprintf("%s:%d", m.pcIP(), m.set.Port))))
	return b.String()
}

func (m *model) hint() string {
	switch m.cursor {
	case itStream:
		return "Enter — включить или остановить приём звука с колонки"
	case itOutput:
		if strings.Contains(strings.ToUpper(m.outs[m.outIdx].Name), "CABLE") {
			return "В Discord выберите микрофон «CABLE Output»"
		}
		return "Для Discord выберите CABLE Input (VB-Cable)"
	case itAGC:
		return "Тихая речь вытягивается, тишина не усиливается"
	case itGain:
		if m.set.AGC {
			return "Верхний предел автогейна"
		}
		return "Постоянный множитель громкости"
	case itPort:
		return "Enter — ввести порт; потом обновите адрес на колонке"
	case itBuffer:
		return "Меньше — ниже задержка, больше — стабильнее по Wi-Fi"
	case itAutostart:
		return "Включать трансляцию сразу при запуске программы"
	case itMonitor:
		return "Живой уровень устройства записи, например CABLE Output"
	case itStation:
		return "IP/порт/пароль SSH, установка и проверка скрипта на колонке"
	case itDiag:
		return "Найти, почему микрофон не работает, и исправить"
	case itAbout:
		return "Описание работы, ссылка на репозиторий и отказ от ответственности"
	}
	return "Настройки сохраняются автоматически"
}

func keyHint(key, desc string) string { return sKey.Render(key) + " " + sDim.Render(desc) }

func (m *model) keys() string {
	if m.edit != nil {
		return keyHint("0-9", "порт") + "   " + keyHint("enter", "сохранить") + "   " + keyHint("esc", "отмена")
	}
	return keyHint("↑↓", "выбор") + "   " + keyHint("←→", "изменить") + "   " + keyHint("enter", "действие") + "   " +
		keyHint("r", "устройства") + "   " + keyHint("q", "выход")
}

func (m *model) viewMonitor() string {
	var b strings.Builder
	b.WriteString(m.header("Проверка микрофона") + "\n" + rule() + "\n\n")
	dev := "—"
	if len(m.ins) > 0 {
		dev = truncate(m.ins[m.inIdx].Name, 40)
	}
	b.WriteString(sDim.Render(padRight("Устройство", 12)) + sDim.Render("‹ ") + sValue.Render(dev) + sDim.Render(" ›") + "\n\n")

	if m.monErr != "" {
		b.WriteString(sBad.Render("✕ "+m.monErr) + "\n")
	} else {
		b.WriteString(sDim.Render(padRight("Уровень", 12)) + meter(m.monShown, 40, true) + "\n")
		b.WriteString(sDim.Render(padRight("", 12)) +
			sDim.Render(fmt.Sprintf("RMS %.0f  ·  пик %d", m.monRMS.Load(), m.monPeak.Load())) + "\n\n")
		if m.monShown > 100 {
			b.WriteString(sPurple.Render("● сигнал есть — звук доходит до микрофона") + "\n")
		} else {
			b.WriteString(sBlue.Render("○ тишина — скажите что-нибудь рядом с колонкой") + "\n")
		}
	}
	b.WriteString("\n" + rule() + "\n")
	if !m.engine.Running() {
		b.WriteString(sDim.Render("Трансляция выключена — включите её, чтобы проверить CABLE") + "\n")
	} else {
		b.WriteString(sDim.Render("Выберите CABLE Output, чтобы проверить виртуальный микрофон") + "\n")
	}
	b.WriteString("\n" + keyHint("←→", "устройство") + "   " + keyHint("r", "обновить") + "   " + keyHint("esc", "назад"))
	return b.String()
}

func (m *model) viewDisclaimer() string {
	var b strings.Builder
	b.WriteString(gradientText("AlisaMic") + "  " + sDim.Render(version) + "  " + sPurple.Render("О ПРОГРАММЕ (ABOUT)") + "\n" + rule() + "\n")
	b.WriteString(sWarn.Render(testedOnly) + "\n\n")
	wrap := lipgloss.NewStyle().Width(panelWidth)
	for _, p := range disclaimerParagraphs {
		b.WriteString(wrap.Render(p) + "\n\n")
	}
	b.WriteString(rule() + "\n")
	if m.disclaimerRO {
		b.WriteString(keyHint("esc", "назад"))
	} else {
		b.WriteString(sBad.Render("Y") + sDim.Render(" — я прочитал, понимаю риски и принимаю их     ") + keyHint("q", "выход"))
	}
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
