package main

// Встроенная диагностика: проверяет ПК, колонку и сам поток звука, а для найденных проблем
// предлагает кнопку «исправить».

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type chkStatus int

const (
	chkOK chkStatus = iota
	chkWarn
	chkFail
	chkSkip
)

// Fix — действие, которое чинит найденную проблему.
type Fix struct {
	Label   string
	Confirm []string // если не пусто — сначала спросить
	Run     func(l *opLog) error
	After   func(m *model) tea.Cmd // на UI-потоке, после успешного Run
	Reboot  bool                   // после успеха предложить перезагрузить колонку
}

type Check struct {
	Group, Name string
	St          chkStatus
	Detail      string
	Fix         *Fix
}

type diagInput struct {
	Set      Settings
	Cfg      SSHConfig
	HaveHost bool
	Outs     []Device
	Ins      []Device
	Running  bool
	Signal   bool
	InLevel  float64
	Sender   string
}

func freePort(from int) int {
	for p := from; p < from+200 && p < 65535; p++ {
		if portFree(p) {
			return p
		}
	}
	return 0
}

// runDiag выполняет все проверки. Блокирует примерно на 3–6 секунд (SSH и прослушивание UDP).
func runDiag(in diagInput) ([]Check, *StationStatus) {
	var cs []Check
	add := func(group, name string, st chkStatus, detail string, fix *Fix) {
		cs = append(cs, Check{group, name, st, detail, fix})
	}
	const gPC, gSt, gSnd = "Этот ПК", "Колонка", "Звук"

	// ── ПК ──
	port := in.Set.Port
	portName := fmt.Sprintf("Порт UDP %d", port)
	portBusy := false
	switch {
	case in.Running:
		add(gPC, portName, chkOK, "слушает AlisaMic", nil)
	case portFree(port):
		add(gPC, portName, chkOK, "свободен", nil)
	default:
		portBusy = true
		np := freePort(port + 1)
		var fix *Fix
		if np != 0 {
			fix = &Fix{
				Label: fmt.Sprintf("Использовать свободный порт %d", np),
				After: func(m *model) tea.Cmd {
					m.set.Port = np
					m.save()
					return nil
				},
			}
		}
		add(gPC, portName, chkFail, "занят другой программой (возможно, второй AlisaMic). Закройте её или выберите другой порт — после смены порта нажмите «Установить/обновить» в меню колонки", fix)
	}

	if runtime.GOOS == "windows" {
		ok, err := firewallRuleExists()
		switch {
		case err != nil:
			add(gPC, "Брандмауэр Windows", chkSkip, "не удалось проверить: "+err.Error(), nil)
		case ok:
			add(gPC, "Брандмауэр Windows", chkOK, "правило для AlisaMic есть", nil)
		default:
			add(gPC, "Брандмауэр Windows", chkWarn,
				"нет разрешающего правила — Windows может тихо блокировать входящий звук от колонки (частая причина «тишины»)",
				&Fix{Label: "Добавить правило (запросит права администратора)", Run: func(l *opLog) error {
					l.add("добавляю правило брандмауэра для UDP…")
					if err := addFirewallRule(); err != nil {
						return err
					}
					l.add("правило добавлено")
					return nil
				}})
		}
	}

	hasIn, hasOut := FindDevice(in.Ins, "CABLE Output") >= 0, FindDevice(in.Outs, "CABLE Input") >= 0
	switch {
	case hasIn && hasOut:
		add(gPC, "VB-Cable", chkOK, "CABLE Input и CABLE Output найдены", nil)
	default:
		add(gPC, "VB-Cable", chkWarn,
			"не найден (для микрофона в Discord/играх нужен; для прослушивания через колонки — нет). После установки перезагрузите ПК",
			&Fix{Label: "Открыть страницу загрузки VB-Cable", Run: func(l *opLog) error {
				l.add("открываю vb-audio.com/Cable в браузере…")
				return openURL("https://vb-audio.com/Cable/")
			}})
	}

	if in.Set.Output == "" {
		add(gPC, "Устройство вывода", chkOK, "колонки по умолчанию", nil)
	} else if i := FindDevice(in.Outs, in.Set.Output); i >= 0 {
		add(gPC, "Устройство вывода", chkOK, in.Outs[i].Name, nil)
	} else {
		add(gPC, "Устройство вывода", chkFail, "«"+in.Set.Output+"» не найдено в системе",
			&Fix{Label: "Выбрать колонки по умолчанию", After: func(m *model) tea.Cmd {
				m.set.Output = ""
				m.outIdx = 0
				m.save()
				return nil
			}})
	}

	// ── колонка ──
	var st *StationStatus
	var rem *Remote
	if !in.HaveHost {
		add(gSt, "Связь по SSH", chkSkip, "не указан IP колонки — «Колонка (SSH)» в главном меню", nil)
	} else {
		var err error
		st, rem, err = FetchStatus(in.Cfg)
		if err != nil {
			var fix *Fix
			if hk, ok := err.(*errHostKeyChanged); ok {
				a := hk.addr
				fix = &Fix{Label: "Забыть сохранённый ключ колонки", Run: func(l *opLog) error {
					rememberHostKey(a, "")
					l.add("ключ забыт — при следующем подключении запомню новый")
					return nil
				}}
			}
			add(gSt, "Связь по SSH", chkFail, err.Error(), fix)
			st = nil
		} else {
			rem.Close()
			add(gSt, "Связь по SSH", chkOK, fmt.Sprintf("%s@%s · %s · аптайм %s", in.Cfg.User, in.Cfg.addr(), st.s("arch"), fmtUptime(st.n("uptime"))), nil)
		}
	}

	skipSt := func(name string) { add(gSt, name, chkSkip, "нет связи с колонкой", nil) }
	pcIP := in.Set.PCIP
	if st != nil && pcIP == "" {
		pcIP = st.LocalIP
	}
	if pcIP == "" {
		pcIP = localIPFor(in.Cfg.Host)
	}
	want := net.JoinHostPort(pcIP, strconv.Itoa(port))
	installFix := func(label string) *Fix {
		cfg := in.Cfg
		return &Fix{Label: label, Reboot: true,
			Confirm: []string{"Будет запущен установщик на колонке (с бэкапом launch.json)."},
			Run:     func(l *opLog) error { return InstallOnStation(cfg, pcIP, port, l) }}
	}

	if st == nil {
		for _, n := range []string{"Прошивка", "Скрипт AlisaMic", "Адрес приёмника", "Загрузка в maind", "Системный раздел", "Место на /data"} {
			skipSt(n)
		}
	} else {
		switch {
		case st.n("uid") != 0:
			add(gSt, "Права", chkWarn, fmt.Sprintf("вы вошли не под root (uid=%d) — установка и настройки не сработают", st.n("uid")), nil)
		}
		switch {
		case !st.b("evil_lib"):
			add(gSt, "Прошивка", chkFail, "нет libyandex_evil.so — это не Яндекс Станция Лайт?", nil)
		case !st.b("vqe_symbol"):
			add(gSt, "Прошивка", chkFail, "в libyandex_evil.so нет YandexVqeC_GetOutputChannelData — эта прошивка не поддерживается", nil)
		default:
			add(gSt, "Прошивка", chkOK, "VQE найден — совместима", nil)
		}

		switch {
		case !st.b("so"):
			add(gSt, "Скрипт AlisaMic", chkFail, "не установлен (libvqe_tap.so нет)", installFix("Установить скрипт"))
		case !st.b("patched"):
			add(gSt, "Скрипт AlisaMic", chkFail, "файл есть, но launch.json не пропатчен (после обновления прошивки?)", installFix("Установить скрипт заново"))
		case !st.UpToDate():
			add(gSt, "Скрипт AlisaMic", chkWarn, "установлена другая версия libvqe_tap.so", installFix("Обновить скрипт"))
		default:
			add(gSt, "Скрипт AlisaMic", chkOK, "установлен, версия актуальна", nil)
		}

		switch {
		case !st.b("patched"):
			add(gSt, "Адрес приёмника", chkSkip, "скрипт не установлен", nil)
		case pcIP == "?":
			add(gSt, "Адрес приёмника", chkWarn, "колонка шлёт на "+st.s("target")+", а IP этого ПК определить не удалось — укажите его в «Колонка (SSH)»", nil)
		case st.s("target") == want:
			add(gSt, "Адрес приёмника", chkOK, "колонка шлёт на "+want+" (этот ПК)", nil)
		default:
			add(gSt, "Адрес приёмника", chkFail, fmt.Sprintf("колонка шлёт на %s, а этот ПК — %s. Так бывает, когда роутер выдал ПК новый IP", orDash(st.s("target")), want),
				installFix("Направить звук на этот ПК"))
		}

		switch {
		case !st.b("patched"):
			add(gSt, "Загрузка в maind", chkSkip, "скрипт не установлен", nil)
		case !st.b("maind"):
			add(gSt, "Загрузка в maind", chkWarn, "процесс maind не найден — колонка ещё грузится или Алиса не запущена", nil)
		case st.b("loaded"):
			add(gSt, "Загрузка в maind", chkOK, "libvqe_tap.so загружен в голосовой движок", nil)
		default:
			cfg := in.Cfg
			add(gSt, "Загрузка в maind", chkWarn, "скрипт установлен, но ещё не подхвачен — колонку нужно перезагрузить",
				&Fix{Label: "Перезагрузить колонку", Confirm: []string{"Алиса отключится на 1–2 минуты."},
					Run: func(l *opLog) error { return RebootStation(cfg, l, true) }})
		}

		if st.s("rootfs") == "rw" {
			cfg := in.Cfg
			add(gSt, "Системный раздел", chkWarn, "смонтирован на запись (обычно только чтение) — вероятно, кто-то оставил remount rw",
				&Fix{Label: "Вернуть режим «только чтение»", Run: func(l *opLog) error {
					r, err := DialSSH(cfg, 8*time.Second)
					if err != nil {
						return err
					}
					defer r.Close()
					out, code, err := r.Run("sync; mount -o remount,ro / 2>&1", nil, 15*time.Second)
					l.addBlock(out)
					if err != nil || code != 0 {
						return fmt.Errorf("не удалось: %v %s", err, strings.TrimSpace(out))
					}
					return nil
				}})
		} else {
			add(gSt, "Системный раздел", chkOK, "только чтение (штатно)", nil)
		}

		free := st.n("data_free_kb")
		switch {
		case st.s("data_free_kb") == "":
			add(gSt, "Место на /data", chkSkip, "не удалось определить", nil)
		case free < 2048:
			add(gSt, "Место на /data", chkFail, fmt.Sprintf("свободно всего %d КБ", free), nil)
		case free < 10240:
			add(gSt, "Место на /data", chkWarn, fmt.Sprintf("свободно %.1f МБ", float64(free)/1024), nil)
		default:
			add(gSt, "Место на /data", chkOK, fmt.Sprintf("свободно %d МБ", free/1024), nil)
		}
	}

	// ── звук ──
	switch {
	case in.Running:
		if in.Signal {
			detail := "идут от " + in.Sender
			if in.HaveHost && in.Sender != "" && in.Sender != in.Cfg.Host {
				add(gSnd, "Пакеты от колонки", chkWarn, "идут от "+in.Sender+", а колонка — "+in.Cfg.Host+". Звук присылает кто-то другой?", nil)
			} else {
				add(gSnd, "Пакеты от колонки", chkOK, detail, nil)
			}
		} else {
			add(gSnd, "Пакеты от колонки", chkFail, noPacketsHint(st, want), nil)
		}
	case portBusy:
		add(gSnd, "Пакеты от колонки", chkSkip, "порт занят — сначала исправьте его", nil)
	default:
		n, from, err := probeUDP(port, 3*time.Second)
		switch {
		case err != nil:
			add(gSnd, "Пакеты от колонки", chkSkip, err.Error(), nil)
		case n > 0:
			add(gSnd, "Пакеты от колонки", chkWarn, fmt.Sprintf("звук приходит (%d пак. за 3 с от %s), но трансляция в AlisaMic выключена", n, from),
				&Fix{Label: "Включить трансляцию", After: func(m *model) tea.Cmd { return m.startCmd() }})
		default:
			add(gSnd, "Пакеты от колонки", chkFail, "за 3 с не пришло ни одного пакета. "+noPacketsHint(st, want), nil)
		}
	}

	switch {
	case in.Running && in.Signal && in.InLevel > 100:
		add(gSnd, "Уровень микрофона", chkOK, fmt.Sprintf("есть сигнал (RMS %.0f)", in.InLevel), nil)
	case in.Running && in.Signal:
		add(gSnd, "Уровень микрофона", chkWarn, fmt.Sprintf("тишина (RMS %.0f). Скажите что-нибудь рядом с колонкой; проверьте, что микрофон на колонке не выключен кнопкой", in.InLevel), nil)
	default:
		add(gSnd, "Уровень микрофона", chkSkip, "нужна работающая трансляция со звуком", nil)
	}
	return cs, st
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtUptime(sec int) string {
	switch {
	case sec >= 86400:
		return fmt.Sprintf("%d д %d ч", sec/86400, sec%86400/3600)
	case sec >= 3600:
		return fmt.Sprintf("%d ч %d мин", sec/3600, sec%3600/60)
	}
	return fmt.Sprintf("%d мин", sec/60)
}

func noPacketsHint(st *StationStatus, want string) string {
	var tips []string
	if st == nil {
		tips = append(tips, "связь с колонкой не проверена")
	} else {
		if !st.Installed() {
			tips = append(tips, "скрипт на колонке не установлен")
		} else if !st.b("loaded") {
			tips = append(tips, "колонку нужно перезагрузить")
		} else if st.s("target") != want {
			tips = append(tips, "колонка шлёт не на этот ПК")
		}
	}
	tips = append(tips, "разрешите UDP в брандмауэре", "ПК и колонка должны быть в одной сети (не гостевая Wi-Fi, без изоляции клиентов)")
	return strings.Join(tips, "; ")
}
