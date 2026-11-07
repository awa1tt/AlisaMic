package main

// Запасной канал связи с колонкой через системный OpenSSH (ssh.exe есть в Windows 10/11).
// Нужен на случай, когда встроенный клиент не договаривается с конкретной сборкой dropbear
// на колонке, а обычный `ssh` из командной строки — договаривается.
// Пароль ssh получает через SSH_ASKPASS: ssh запускает этот же exe с ALISAMIC_ASKPASS=1.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	sysMu   sync.Mutex
	sysPref = map[string]bool{} // адреса, где встроенный клиент не работает
	sshLogM sync.Mutex
)

// sshLog пишет подробности в alisa-mic-ssh.log только при включённом ALISAMIC_DEBUG=1.
func sshLog(format string, a ...any) {
	if os.Getenv("ALISAMIC_DEBUG") != "1" {
		return
	}
	sshLogM.Lock()
	defer sshLogM.Unlock()
	p := pickConfigPath("alisa-mic-ssh.log")
	if p == "" {
		return
	}
	if st, err := os.Stat(p); err == nil && st.Size() > 200_000 {
		_ = os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, a...))
}

func sshLogPath() string { return pickConfigPath("alisa-mic-ssh.log") }

func sysPreferred(cfg SSHConfig) bool {
	if os.Getenv("ALISAMIC_FORCE_SYS") == "1" { // для тестов
		return true
	}
	sysMu.Lock()
	defer sysMu.Unlock()
	return sysPref[cfg.addr()]
}

func setSysPreferred(cfg SSHConfig, v bool) {
	sysMu.Lock()
	sysPref[cfg.addr()] = v
	sysMu.Unlock()
}

// findSSH ищет ssh.exe / ssh.
func findSSH() string {
	if p, err := exec.LookPath("ssh"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, env := range []string{"SystemRoot", "windir"} {
			if root := os.Getenv(env); root != "" {
				p := filepath.Join(root, "System32", "OpenSSH", "ssh.exe")
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	return ""
}

// askpassMain вызывается, когда ssh запустил нас как SSH_ASKPASS: печатает пароль и выходит.
func askpassMain() {
	prompt := strings.ToLower(strings.Join(os.Args[1:], " "))
	if strings.Contains(prompt, "yes/no") || strings.Contains(prompt, "fingerprint") {
		fmt.Println("yes")
	} else {
		fmt.Println(os.Getenv("ALISAMIC_PW"))
	}
	os.Exit(0)
}

func sysRun(cfg SSHConfig, cmd string, stdin []byte, timeout time.Duration) (string, int, error) {
	return sysRunStream(cfg, cmd, stdin, timeout, nil)
}

func sysRunStream(cfg SSHConfig, cmd string, stdin []byte, timeout time.Duration, onLine func(string)) (string, int, error) {
	bin := findSSH()
	if bin == "" {
		return "", -1, errors.New("в системе нет ssh (OpenSSH-клиента)")
	}
	exe, err := os.Executable()
	if err != nil {
		return "", -1, err
	}
	null := "/dev/null"
	if runtime.GOOS == "windows" {
		null = "NUL"
	}
	secs := int(timeout.Seconds())
	if secs > 10 {
		secs = 10
	}
	if secs < 3 {
		secs = 3
	}
	sshLog("→ системный ssh: %s %s@%s -p %d", bin, cfg.User, cfg.Host, cfg.Port)
	args := []string{
		"-p", strconv.Itoa(cfg.Port), "-T",
		"-o", "NumberOfPasswordPrompts=1",
		"-o", "PreferredAuthentications=password,keyboard-interactive",
		"-o", "PubkeyAuthentication=no",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=" + null,
		"-o", "LogLevel=ERROR",
		"-o", "GSSAPIAuthentication=no",
		"-o", "AddressFamily=inet",
		"-o", "ConnectTimeout=" + strconv.Itoa(secs),
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=6",
	}
	if stdin == nil {
		args = append(args, "-n")
	}
	args = append(args, cfg.User+"@"+cfg.Host, cmd)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	// ssh.exe ищет askpass через posix_spawnp по PATH и не понимает путь с обратными
	// слешами, поэтому кладём каталог exe в начало PATH, а в SSH_ASKPASS пишем только имя.
	env := os.Environ()
	dir := filepath.Dir(exe)
	for i, e := range env {
		if len(e) >= 5 && strings.EqualFold(e[:5], "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + e[5:]
			break
		}
	}
	env = append(env,
		"SSH_ASKPASS="+filepath.Base(exe),
		"SSH_ASKPASS_REQUIRE=force", "DISPLAY=alisamic:0",
		"ALISAMIC_ASKPASS=1", "ALISAMIC_PW="+cfg.Password)
	c.Env = env
	sshLog("системный ssh: askpass=%s (каталог %s)", filepath.Base(exe), dir)
	hideChild(c)
	c.WaitDelay = 2 * time.Second
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	} else {
		c.Stdin = strings.NewReader("")
	}
	var lw lineWriter
	lw.onLine = onLine
	c.Stdout, c.Stderr = &lw, &lw
	err = c.Run()
	lw.Flush()
	s := lw.String()
	if len(s) > 4000 {
		s = s[:4000]
	}
	sshLog("системный ssh %s (код=%v):\n%s", cfg.addr(), err, s)
	if ctx.Err() == context.DeadlineExceeded {
		return lw.String(), -1, fmt.Errorf("команда не завершилась за %d с", int(timeout.Seconds()))
	}
	if err == nil {
		return lw.String(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code == 255 { // ssh сам не смог подключиться
			return lw.String(), -1, sysErr(lw.String(), cfg)
		}
		return lw.String(), code, nil
	}
	return lw.String(), -1, err
}

func sysErr(out string, cfg SSHConfig) error {
	o := strings.TrimSpace(out)
	l := strings.ToLower(o)
	switch {
	case strings.Contains(l, "permission denied"):
		return fmt.Errorf("неверный логин или пароль (%s)", cfg.User)
	case strings.Contains(l, "connection refused"):
		return fmt.Errorf("%s:%d закрыт — на колонке не запущен SSH или неверный порт", cfg.Host, cfg.Port)
	case strings.Contains(l, "timed out"):
		return fmt.Errorf("колонка %s не отвечает — проверьте IP и сеть", cfg.Host)
	}
	if len(o) > 300 {
		o = o[:300]
	}
	if o == "" {
		o = "ssh завершился с ошибкой"
	}
	return errors.New("ssh: " + o)
}

func isAuthErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "неверный логин или пароль")
}

// dialSys проверяет, что системный ssh умеет подключаться, и возвращает Remote в «системном» режиме.
func dialSys(cfg SSHConfig, timeout time.Duration) (*Remote, error) {
	out, code, err := sysRun(cfg, "echo alisamic-ok", nil, timeout+5*time.Second)
	if err != nil {
		return nil, err
	}
	if code != 0 || !strings.Contains(out, "alisamic-ok") {
		return nil, fmt.Errorf("ssh: неожиданный ответ колонки: %s", strings.TrimSpace(out))
	}
	return &Remote{sys: true, cfg: cfg, LocalIP: localIPFor(cfg.Host)}, nil
}
