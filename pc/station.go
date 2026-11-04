package main

// Операции над колонкой: проверка, установка скрипта, откат, смена порта SSH и пароля, перезагрузка.

import (
	"crypto/md5"
	"crypto/rand"
	_ "embed"
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

type opLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *opLog) add(s string) {
	l.mu.Lock()
	l.lines = append(l.lines, s)
	l.mu.Unlock()
}

func (l *opLog) addBlock(s string) {
	for _, ln := range strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "\r", ""), "\n") {
		if ln = strings.TrimRight(ln, " \t"); ln != "" {
			l.add("  " + ln)
		}
	}
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
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
	r, err := DialSSH(cfg, 8*time.Second)
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
	out, _, err := r.RunScript("status.sh", statusSh, 20*time.Second)
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
	out, code, err := r.RunScript(name, body, timeout, args...)
	if err != nil {
		return nil, err
	}
	var shown []string
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if strings.HasPrefix(ln, "RESULT=") || strings.HasPrefix(ln, "OLD=") || strings.TrimSpace(ln) == "" {
			continue
		}
		shown = append(shown, ln)
	}
	log.addBlock(strings.Join(shown, "\n"))
	kv := parseKV(out)
	if kv["RESULT"] != "OK" {
		msg := "скрипт на колонке завершился с ошибкой"
		for _, ln := range shown {
			if strings.HasPrefix(ln, "ОШИБКА") {
				msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(ln, "ОШИБКА:"), "ОШИБКА"))
			}
		}
		if code != 0 && len(shown) == 0 {
			msg = fmt.Sprintf("скрипт завершился с кодом %d без вывода", code)
		}
		return kv, errors.New(msg)
	}
	return kv, nil
}

// InstallOnStation ставит/обновляет libvqe_tap.so и указывает колонке слать звук на pcIP:port.
func InstallOnStation(cfg SSHConfig, pcIP string, port int, log *opLog) error {
	log.add("подключаюсь к " + cfg.addr() + "…")
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return err
	}
	defer r.Close()
	log.add("подключено как " + cfg.User)

	log.add("проверяю конфигурацию колонки и создаю бэкап…")
	if out, code, err := r.Run("cat /system/vendor/quasar/launch.json 2>/dev/null", nil, 10*time.Second); err == nil && code == 0 && len(strings.TrimSpace(out)) > 0 {
		backupPath := filepath.Join(filepath.Dir(configPath()), "station-launch.json.bak")
		if _, err := os.Stat(backupPath); os.IsNotExist(err) {
			_ = os.WriteFile(backupPath, []byte(out), 0o600)
			log.add("локальная копия конфига сохранена: " + filepath.Base(backupPath))
		}
	}

	pkg := "/tmp/alisamic-pkg"
	if _, _, err := r.Run("rm -rf "+pkg+" && mkdir -p "+pkg, nil, 10*time.Second); err != nil {
		return err
	}
	defer r.Run("rm -rf "+pkg, nil, 10*time.Second)

	log.add("загружаю libvqe_tap.so и установщик…")
	if err := r.Upload(pkg+"/libvqe_tap.so", tapSO, "755"); err != nil {
		return err
	}
	if err := r.Upload(pkg+"/install.sh", []byte(installSh), "755"); err != nil {
		return err
	}
	// проверка целостности загрузки
	if out, _, _ := r.Run("md5sum "+pkg+"/libvqe_tap.so 2>/dev/null | cut -d' ' -f1", nil, 10*time.Second); strings.TrimSpace(out) != "" {
		if strings.TrimSpace(out) != tapMD5() {
			return errors.New("libvqe_tap.so повредился при загрузке (не совпала контрольная сумма) — повторите")
		}
	}
	log.add("запускаю установщик на колонке (создаётся бэкап конфига)…")
	_, err = runChecked(r, "run-install.sh", "#!/bin/sh\nexec sh "+pkg+"/install.sh \"$@\"\n", log, 60*time.Second, pcIP, strconv.Itoa(port))
	return err
}

func UninstallFromStation(cfg SSHConfig, log *opLog) error {
	log.add("подключаюсь к " + cfg.addr() + "…")
	r, err := DialSSH(cfg, 8*time.Second)
	if err != nil {
		return err
	}
	defer r.Close()
	log.add("возвращаю launch.json из бэкапа и очищаю файлы…")
	_, err = runChecked(r, "uninstall.sh", uninstallSh, log, 60*time.Second)
	if err == nil {
		log.add("конфигурация колонки успешно возвращена из бэкапа")
	}
	return err
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
