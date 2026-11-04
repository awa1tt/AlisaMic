package main

// Операции над колонкой: проверка, установка скрипта, откат, смена порта SSH и пароля, перезагрузка.

import (
	"crypto/md5"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GehirnInc/crypt/sha512_crypt"
)

//go:embed embed/libvqe_tap.so
var tapSO []byte

//go:embed embed/install.sh
var installSh string

//go:embed embed/uninstall.sh
var uninstallSh string

//go:embed embed/status.sh
var statusSh string

//go:embed embed/sshport.sh
var sshPortSh string

//go:embed embed/passwd.sh
var passwdSh string

// на случай, если git/архиватор на Windows превратил переводы строк в CRLF — для sh на колонке нужен LF
func lf(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func init() {
	installSh, uninstallSh, statusSh = lf(installSh), lf(uninstallSh), lf(statusSh)
	sshPortSh, passwdSh = lf(sshPortSh), lf(passwdSh)
}

func tapMD5() string { s := md5.Sum(tapSO); return hex.EncodeToString(s[:]) }

// ───────────────────────── журнал операции ─────────────────────────

type opStepStatus int

const (
	stepPending opStepStatus = iota
	stepRunning
	stepDone
	stepFail
)

type opStep struct {
	Label  string
	Desc   string
	Status opStepStatus
}

type opLog struct {
	mu    sync.Mutex
	lines []string
	steps []opStep
}

func (l *opLog) add(s string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if len(l.lines) > 0 && l.lines[len(l.lines)-1] == s {
		return
	}
	l.lines = append(l.lines, s)
}

func (l *opLog) addBlock(s string) {
	if l == nil {
		return
	}
	for _, ln := range strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r", ""), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			l.add(ln)
		}
	}
}

func (l *opLog) snapshot() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func (l *opLog) setSteps(steps []opStep) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append([]opStep(nil), steps...)
}

func (l *opLog) startStep(idx int, desc string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx >= 0 && idx < len(l.steps) {
		l.steps[idx].Status = stepRunning
		if desc != "" {
			l.steps[idx].Desc = desc
		}
	}
}

func (l *opLog) doneStep(idx int, desc string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx >= 0 && idx < len(l.steps) {
		l.steps[idx].Status = stepDone
		if desc != "" {
			l.steps[idx].Desc = desc
		}
	}
}

func (l *opLog) failStep(idx int, desc string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx >= 0 && idx < len(l.steps) {
		l.steps[idx].Status = stepFail
		if desc != "" {
			l.steps[idx].Desc = desc
		}
	}
}

func (l *opLog) getSteps() []opStep {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]opStep(nil), l.steps...)
}

// ───────────────────────── статус ─────────────────────────

type StationStatus struct {
	kv      map[string]string
	LocalIP string // IP этого ПК, как его видит колонка
	When    time.Time
}

func (s *StationStatus) s(k string) string { return s.kv[k] }
func (s *StationStatus) b(k string) bool   { return s.kv[k] == "1" }
func (s *StationStatus) n(k string) int    { v, _ := strconv.Atoi(s.kv[k]); return v }

func parseKV(out string) map[string]string {
	kv := map[string]string{}
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if i := strings.IndexByte(ln, '='); i > 0 && !strings.ContainsAny(ln[:i], " \t") {
			kv[ln[:i]] = strings.TrimSpace(ln[i+1:])
		}
	}
	return kv
}

func (s *StationStatus) Installed() bool { return s.b("so") && s.b("patched") }
func (s *StationStatus) UpToDate() bool  { return s.b("so") && s.s("so_md5") == tapMD5() }

// SSHPortOnStation — порт, который колонка слушает по init-скрипту (может отличаться от текущего).
func (s *StationStatus) SSHPortInit() int { return s.n("ssh_init_port") }

func FetchStatus(cfg SSHConfig) (*StationStatus, *Remote, error) {
	r, err := DialSSH(cfg, 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	st, err := fetchStatusOn(r)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	return st, r, nil
}

func fetchStatusOn(r *Remote) (*StationStatus, error) {
	out, _, err := r.RunScript("status.sh", statusSh, 30*time.Second)
	if err != nil {
		return nil, err
	}
	kv := parseKV(out)
	if kv["END"] != "1" {
		return nil, fmt.Errorf("колонка вернула неожиданный ответ: %s", firstLine(out))
	}
	return &StationStatus{kv: kv, LocalIP: r.LocalIP, When: time.Now()}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len([]rune(s)) > 80 {
		s = string([]rune(s)[:80]) + "…"
	}
	return s
}

// ───────────────────────── установка / откат ─────────────────────────

// runChecked запускает скрипт и считает успехом только строку RESULT=OK.
func runChecked(r *Remote, name, body string, log *opLog, timeout time.Duration, args ...string) (map[string]string, error) {
	return runCheckedStream(r, name, body, log, timeout, nil, args...)
}

func runCheckedStream(r *Remote, name, body string, log *opLog, timeout time.Duration, onLine func(string), args ...string) (map[string]string, error) {
	var inBackup bool
	var backupLines []string
	var shown []string

	wrappedOnLine := func(line string) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "LAUNCH_BACKUP_BEGIN" {
			inBackup = true
			return
		}
		if trimmed == "LAUNCH_BACKUP_END" {
			inBackup = false
			if len(backupLines) > 0 {
				b64Str := strings.Join(backupLines, "")
				if data, err := base64.StdEncoding.DecodeString(b64Str); err == nil && len(data) > 0 {
					backupPath := filepath.Join(filepath.Dir(configPath()), "station-launch.json.bak")
					_ = os.WriteFile(backupPath, data, 0o600)
					if log != nil {
						log.add("локальный бэкап сохранён: " + filepath.Base(backupPath))
					}
				}
			}
			return
		}
		if inBackup {
			backupLines = append(backupLines, trimmed)
			return
		}

		if onLine != nil {
			onLine(line)
		}
		if !strings.HasPrefix(trimmed, "RESULT=") && !strings.HasPrefix(trimmed, "OLD=") && !strings.HasPrefix(trimmed, "[STEP:") && trimmed != "" {
			shown = append(shown, trimmed)
			if log != nil {
				log.add(trimmed)
			}
		}
	}

	out, code, err := r.RunScriptStream(name, body, timeout, wrappedOnLine, args...)
	if err != nil {
		return nil, err
	}
	kv := parseKV(out)
	if kv["RESULT"] != "OK" {
		msg := "скрипт на колонке завершился с ошибкой"
		for _, ln := range strings.Split(out, "\n") {
			ln = strings.TrimSpace(ln)
			if strings.HasPrefix(ln, "ОШИБКА") {
				msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(ln, "ОШИБКА:"), "ОШИБКА"))
			}
		}
		if code != 0 && msg == "скрипт на колонке завершился с ошибкой" {
			msg = fmt.Sprintf("скрипт завершился с кодом %d", code)
		}
		return kv, errors.New(msg)
	}
	return kv, nil
}

// InstallOnStation ставит/обновляет libvqe_tap.so и указывает колонке слать звук на pcIP:port.
func InstallOnStation(cfg SSHConfig, pcIP string, port int, log *opLog) error {
	steps := []opStep{
		{Label: "Связь", Desc: "подключение к " + cfg.addr(), Status: stepRunning},
		{Label: "Прошивка", Desc: "проверка VQE и aarch64", Status: stepPending},
		{Label: "Бэкап", Desc: "сохранение launch.json", Status: stepPending},
		{Label: "Установка", Desc: "загрузка libvqe_tap.so", Status: stepPending},
		{Label: "Конфиг", Desc: fmt.Sprintf("звук на %s:%d", pcIP, port), Status: stepPending},
		{Label: "Проверка", Desc: "проверка прав и launch.json", Status: stepPending},
	}
	log.setSteps(steps)
	log.add("подключаюсь к " + cfg.addr() + "…")

	r, err := DialSSH(cfg, 12*time.Second)
	if err != nil {
		log.failStep(0, err.Error())
		return err
	}
	defer r.Close()
	log.doneStep(0, fmt.Sprintf("%s@%s (ОК)", cfg.User, cfg.addr()))
	log.startStep(1, "проверка прошивки и совместимости…")

	b64SO := base64.StdEncoding.EncodeToString(tapSO)
	fullScript := fmt.Sprintf("#!/bin/sh\nEMBEDDED_SO_B64=\"%s\"\n%s", b64SO, installSh)

	onLine := func(line string) {
		switch {
		case strings.HasPrefix(line, "[STEP:fw:start]"):
			log.startStep(1, "проверка VQE и aarch64…")
		case strings.HasPrefix(line, "[STEP:fw:ok]"):
			log.doneStep(1, "VQE найден, aarch64 совместима")
			log.startStep(2, "создание резервной копии…")
		case strings.HasPrefix(line, "[STEP:fw:fail]"):
			log.failStep(1, strings.TrimPrefix(line, "[STEP:fw:fail] "))
		case strings.HasPrefix(line, "[STEP:backup:start]"):
			log.startStep(2, "сохранение бэкапа launch.json…")
		case strings.HasPrefix(line, "[STEP:backup:ok]"):
			log.doneStep(2, "бэкап сохранён на колонке и ПК")
			log.startStep(3, "установка libvqe_tap.so…")
		case strings.HasPrefix(line, "[STEP:backup:fail]"):
			log.failStep(2, strings.TrimPrefix(line, "[STEP:backup:fail] "))
		case strings.HasPrefix(line, "[STEP:install:start]"):
			log.startStep(3, "установка libvqe_tap.so…")
		case strings.HasPrefix(line, "[STEP:install:ok]"):
			log.doneStep(3, "libvqe_tap.so установлен (MD5 OK)")
			log.startStep(4, "настройка приёмника звука…")
		case strings.HasPrefix(line, "[STEP:install:fail]"):
			log.failStep(3, strings.TrimPrefix(line, "[STEP:install:fail] "))
		case strings.HasPrefix(line, "[STEP:target:start]"):
			log.startStep(4, fmt.Sprintf("звук на %s:%d…", pcIP, port))
		case strings.HasPrefix(line, "[STEP:target:ok]"):
			log.doneStep(4, fmt.Sprintf("звук на %s:%d", pcIP, port))
			log.startStep(5, "проверка прав и валидация…")
		case strings.HasPrefix(line, "[STEP:target:fail]"):
			log.failStep(4, strings.TrimPrefix(line, "[STEP:target:fail] "))
		case strings.HasPrefix(line, "[STEP:verify:start]"):
			log.startStep(5, "проверка прав и валидация…")
		case strings.HasPrefix(line, "[STEP:verify:ok]"):
			log.doneStep(5, "конфигурация проверена успешно")
		case strings.HasPrefix(line, "[STEP:verify:fail]"):
			log.failStep(5, strings.TrimPrefix(line, "[STEP:verify:fail] "))
		}
	}

	_, err = runCheckedStream(r, "install.sh", fullScript, log, 60*time.Second, onLine, pcIP, strconv.Itoa(port))
	if err != nil {
		for i, st := range log.getSteps() {
			if st.Status == stepRunning {
				log.failStep(i, err.Error())
				break
			}
		}
		return err
	}
	return nil
}

func UninstallFromStation(cfg SSHConfig, log *opLog) error {
	steps := []opStep{
		{Label: "Связь", Desc: "подключение к " + cfg.addr(), Status: stepRunning},
		{Label: "Бэкап", Desc: "восстановление launch.json", Status: stepPending},
		{Label: "Очистка", Desc: "удаление libvqe_tap.so и файлов", Status: stepPending},
		{Label: "Синхронизация", Desc: "сброс кэша ФС (sync)", Status: stepPending},
	}
	log.setSteps(steps)
	log.add("подключаюсь к " + cfg.addr() + "…")

	r, err := DialSSH(cfg, 12*time.Second)
	if err != nil {
		log.failStep(0, err.Error())
		return err
	}
	defer r.Close()
	log.doneStep(0, fmt.Sprintf("%s@%s (ОК)", cfg.User, cfg.addr()))
	log.startStep(1, "восстановление launch.json из бэкапа…")

	onLine := func(line string) {
		switch {
		case strings.HasPrefix(line, "[STEP:restore:start]"):
			log.startStep(1, "восстановление launch.json…")
		case strings.HasPrefix(line, "[STEP:restore:ok]"):
			log.doneStep(1, "launch.json возвращён из бэкапа")
			log.startStep(2, "удаление файлов…")
		case strings.HasPrefix(line, "[STEP:restore:fail]"):
			log.failStep(1, strings.TrimPrefix(line, "[STEP:restore:fail] "))
		case strings.HasPrefix(line, "[STEP:clean:start]"):
			log.startStep(2, "удаление libvqe_tap.so и файлов…")
		case strings.HasPrefix(line, "[STEP:clean:ok]"):
			log.doneStep(2, "все файлы AlisaMic удалены")
			log.startStep(3, "синхронизация ФС…")
		case strings.HasPrefix(line, "[STEP:clean:fail]"):
			log.failStep(2, strings.TrimPrefix(line, "[STEP:clean:fail] "))
		case strings.HasPrefix(line, "[STEP:sync:start]"):
			log.startStep(3, "синхронизация ФС…")
		case strings.HasPrefix(line, "[STEP:sync:ok]"):
			log.doneStep(3, "файловая система синхронизирована")
		case strings.HasPrefix(line, "[STEP:sync:fail]"):
			log.failStep(3, strings.TrimPrefix(line, "[STEP:sync:fail] "))
		}
	}

	_, err = runCheckedStream(r, "uninstall.sh", uninstallSh, log, 60*time.Second, onLine)
	if err != nil {
		for i, st := range log.getSteps() {
			if st.Status == stepRunning {
				log.failStep(i, err.Error())
				break
			}
		}
		return err
	}
	log.add("конфигурация колонки успешно возвращена из бэкапа")
	return nil
}

// RebootStation перезагружает колонку и (если wait) ждёт, пока она вернётся.
func RebootStation(cfg SSHConfig, log *opLog, wait bool) error {
	log.add("подключаюсь к " + cfg.addr() + "…")
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return err
	}
	log.add("отправляю reboot")
	_, _, err = r.Run("(sleep 1; reboot || reboot -f) </dev/null >/dev/null 2>&1 &", nil, 10*time.Second)
	r.Close()
	if err != nil {
		return err
	}
	if !wait {
		return nil
	}
	if !WaitReboot(cfg, log, 3*time.Minute) {
		return errors.New("колонка не вернулась за 3 минуты — проверьте питание и Wi-Fi")
	}
	return nil
}

// ───────────────────────── SSH-настройки колонки ─────────────────────────

const hashAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func sha512Hash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	for i := range salt {
		salt[i] = hashAlphabet[int(salt[i])%len(hashAlphabet)]
	}
	return sha512_crypt.New().Generate([]byte(password), []byte("$6$"+string(salt)))
}

// ChangeSSHPort меняет порт dropbear. Порядок безопасный: новый слушатель поднимается рядом со
// старым, вход на новом порту проверяется, и только потом старый закрывается. Если проверка не
// прошла — всё откатывается, а старое соединение остаётся рабочим.
func ChangeSSHPort(cfg SSHConfig, newPort int, log *opLog) error {
	if newPort == cfg.Port {
		return errors.New("порт уже такой")
	}
	log.add("подключаюсь к " + cfg.addr() + "…")
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return err
	}
	defer r.Close()
	np := strconv.Itoa(newPort)
	log.add("поднимаю SSH на порту " + np + " рядом со старым…")
	kv, err := runChecked(r, "sshport.sh", sshPortSh, log, 30*time.Second, "start", np)
	if err != nil {
		return err
	}
	old := kv["OLD"]
	if old == "" {
		old = strconv.Itoa(cfg.Port)
	}
	ncfg := cfg
	ncfg.Port = newPort
	log.add("проверяю вход на новом порту…")
	var r2 *Remote
	for i := 0; i < 3; i++ {
		if r2, err = DialSSH(ncfg, 6*time.Second); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		log.add("вход на новом порту не получился: " + err.Error())
		log.add("откатываю изменения…")
		if _, e := runChecked(r, "sshport.sh", sshPortSh, log, 30*time.Second, "rollback", np); e != nil {
			return fmt.Errorf("не удалось войти на новый порт и откат тоже не прошёл (%v). Старый порт %s пока работает", e, old)
		}
		return fmt.Errorf("не удалось войти на порт %d — изменения откатил, старый порт %s работает", newPort, old)
	}
	defer r2.Close()
	log.add("вход на новом порту работает, закрываю старый порт " + old + "…")
	if _, err := runChecked(r2, "sshport.sh", sshPortSh, log, 30*time.Second, "finish", old); err != nil {
		return fmt.Errorf("порт %d работает, но старый закрыть не вышло: %v", newPort, err)
	}
	return nil
}

// ChangeSSHPassword меняет пароль root на колонке. Сам пароль по сети не отправляется — только SHA-512-хеш.
func ChangeSSHPassword(cfg SSHConfig, newPass string, log *opLog) error {
	if newPass == "" {
		return errors.New("пустой пароль не поддерживается — введите хотя бы один символ")
	}
	if strings.ContainsAny(newPass, "\r\n\x00") {
		return errors.New("в пароле не должно быть переводов строки")
	}
	hash, err := sha512Hash(newPass)
	if err != nil {
		return err
	}
	log.add("подключаюсь к " + cfg.addr() + "…")
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return err
	}
	defer r.Close()
	log.add("записываю новый пароль (старый файл сохраняется в *.bak-alisamic)…")
	if _, err := runChecked(r, "passwd.sh", passwdSh, log, 30*time.Second, "set", hash); err != nil {
		return err
	}
	log.add("проверяю вход с новым паролем…")
	ncfg := cfg
	ncfg.Password = newPass
	r2, err := DialSSH(ncfg, 8*time.Second)
	if err != nil {
		log.add("вход с новым паролем не работает: " + err.Error())
		log.add("откатываю пароль…")
		if _, e := runChecked(r, "passwd.sh", passwdSh, log, 30*time.Second, "rollback"); e != nil {
			return fmt.Errorf("новый пароль не принялся и откат не прошёл (%v). Старый пароль может не работать!", e)
		}
		return errors.New("колонка не приняла новый пароль (dropbear, видимо, берёт его не из /etc/shadow) — старый пароль восстановлен")
	}
	r2.Close()
	return nil
}

// ───────────────────────── информация и логи ─────────────────────────

func StationInfo(cfg SSHConfig) ([]string, error) {
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	script := `echo "== система"; uname -a; echo; echo "== аптайм"; uptime
echo; echo "== память"; head -n 3 /proc/meminfo
echo; echo "== диски"; df -h / /data 2>/dev/null
echo; echo "== сеть"; (ip -4 addr show 2>/dev/null || ifconfig 2>/dev/null) | grep -E 'inet |^[0-9]+:|^[a-z0-9]+ ' | head -n 12
echo; echo "== звуковые карты"; cat /proc/asound/cards 2>/dev/null || echo "нет /proc/asound/cards"
echo; echo "== SSH-сервер"; for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null | grep '^[^ ]*dropbear'; done | head -n 3
echo; echo "== AlisaMic"; ls -la /data/quasar/micstream 2>/dev/null || echo "папки нет — скрипт не установлен"
grep -o 'ALICE_TAP_TARGET=[0-9.:]*' /system/vendor/quasar/launch.json 2>/dev/null
`
	out, _, err := r.RunScript("info.sh", script, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(strings.ReplaceAll(out, "\r", ""), "\n"), "\n"), nil
}

func StationLogs(cfg SSHConfig) ([]string, error) {
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	script := `echo "== признаки работы тапа (ALICE_VQE_TAP_UDP)"
(logread 2>/dev/null; dmesg 2>/dev/null) | grep -i 'ALICE_VQE_TAP\|vqe_tap' | tail -n 5
echo; echo "== dmesg (хвост)"; dmesg 2>/dev/null | tail -n 40
echo; echo "== logread (хвост)"; logread 2>/dev/null | tail -n 40 || echo "logread нет"
echo; echo "== лог dropbear"; tail -n 15 /data/opt/etc/dropbear/dropbear.log 2>/dev/null || echo "нет лога"
`
	out, _, err := r.RunScript("logs.sh", script, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(strings.ReplaceAll(out, "\r", ""), "\n"), "\n"), nil
}
