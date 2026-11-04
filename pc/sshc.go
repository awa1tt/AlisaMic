package main

// SSH-клиент для работы с колонкой. На колонке стоит dropbear, поэтому используем только
// самое простое: exec-запросы и пароль. Каждая операция открывает своё соединение.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHConfig struct {
	Host     string
	Port     int
	User     string
	Password string
}

func (c SSHConfig) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

func (c SSHConfig) valid() error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("не указан IP колонки")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("SSH-порт должен быть от 1 до 65535")
	}
	if strings.TrimSpace(c.User) == "" {
		return errors.New("не указан пользователь")
	}
	return nil
}

// errHostKeyChanged — отпечаток ключа колонки не совпал с запомненным.
type errHostKeyChanged struct{ addr, old, now string }

func (e *errHostKeyChanged) Error() string {
	return "ключ SSH колонки изменился (переустановка/сброс?). Если это ваша колонка — «Забыть ключ колонки» в меню"
}

type Remote struct {
	cli     *ssh.Client
	sys     bool // работаем через системный ssh, а не встроенный клиент
	cfg     SSHConfig
	LocalIP string // IP этого ПК так, как его видит колонка
}

func (r *Remote) Close() {
	if r != nil && r.cli != nil {
		r.cli.Close()
	}
}

// DialSSH подключается к колонке. Сначала встроенный клиент (с проверкой ключа хоста), а если он
// не договорился с dropbear на колонке — системный ssh (как в командной строке).
func DialSSH(cfg SSHConfig, timeout time.Duration) (*Remote, error) {
	if err := cfg.valid(); err != nil {
		return nil, err
	}
	sshLog("▶ подключаюсь к %s (user=%s)", cfg.addr(), cfg.User)
	if sysPreferred(cfg) {
		r, err := dialSys(cfg, timeout)
		if err == nil {
			return r, nil
		}
		sshLog("системный ssh %s: %v", cfg.addr(), err)
		if isAuthErr(err) {
			return nil, err
		}
	}
	r, retry, err := dialGo(cfg, timeout)
	if err == nil {
		sshLog("✔ встроенный клиент подключился к %s", cfg.addr())
		return r, nil
	}
	sshLog("✖ встроенный клиент %s: %v", cfg.addr(), err)
	if !retry {
		return nil, err
	}
	sshLog("→ пробую запасной системный ssh для %s", cfg.addr())
	r2, err2 := dialSys(cfg, timeout)
	if err2 == nil {
		sshLog("%s: встроенный клиент не смог, системный ssh работает — переключаюсь на него", cfg.addr())
		setSysPreferred(cfg, true)
		return r2, nil
	}
	sshLog("системный ssh %s: %v", cfg.addr(), err2)
	if isAuthErr(err2) {
		return nil, err2
	}
	if findSSH() == "" {
		return nil, fmt.Errorf("%v. Системного ssh для запасного способа тоже нет — включите «Клиент OpenSSH» в компонентах Windows", err)
	}
	return nil, fmt.Errorf("%v (запасной способ через системный ssh тоже не вышел: %v). Подробности: %s", err, err2, sshLogPath())
}

// dialGo — встроенный клиент. retry=true, если имеет смысл попробовать системный ssh.
func dialGo(cfg SSHConfig, timeout time.Duration) (*Remote, bool, error) {
	addr := cfg.addr()
	var hkErr *errHostKeyChanged
	conf := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				a := make([]string, len(qs))
				for i := range a {
					a[i] = cfg.Password
				}
				return a, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			old := knownHostKey(addr)
			switch {
			case old == "":
				rememberHostKey(addr, fp)
				return nil
			case old == fp:
				return nil
			}
			hkErr = &errHostKeyChanged{addr, old, fp}
			return hkErr
		},
		Timeout: timeout,
		// старые сборки dropbear знают только «древние» алгоритмы — разрешаем их
		Config: ssh.Config{KeyExchanges: []string{
			"curve25519-sha256", "curve25519-sha256@libssh.org",
			"ecdh-sha2-nistp256", "ecdh-sha2-nistp384", "ecdh-sha2-nistp521",
			"diffie-hellman-group14-sha256", "diffie-hellman-group14-sha1", "diffie-hellman-group1-sha1",
		}},
	}
	nc, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, false, explainNetErr(err, cfg)
	}
	_ = nc.SetDeadline(time.Now().Add(timeout + 5*time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(nc, addr, conf)
	if err != nil {
		nc.Close()
		if hkErr != nil {
			return nil, false, hkErr
		}
		sshLog("handshake %s: сырая ошибка: %v", addr, err)
		// Даже если это «неверный логин/пароль», всё равно пробуем системный ssh:
		// на старых сборках dropbear встроенный клиент может ошибаться там, где обычный ssh работает.
		return nil, true, explainSSHErr(err, cfg)
	}
	_ = nc.SetDeadline(time.Time{})
	r := &Remote{cli: ssh.NewClient(sc, chans, reqs), cfg: cfg}
	if la, ok := nc.LocalAddr().(*net.TCPAddr); ok {
		r.LocalIP = la.IP.String()
	}
	// keepalive, чтобы Wi-Fi не рвал долгие операции
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for range t.C {
			if _, _, err := r.cli.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}()
	return r, false, nil
}

func explainNetErr(err error, cfg SSHConfig) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "refused") || strings.Contains(s, "отказ") || strings.Contains(s, "No connection could be made"):
		return fmt.Errorf("%s:%d закрыт — на колонке не запущен SSH или неверный порт", cfg.Host, cfg.Port)
	case strings.Contains(s, "timeout") || strings.Contains(s, "timed out") || strings.Contains(s, "i/o timeout"):
		return fmt.Errorf("колонка %s не отвечает — проверьте IP, что она включена и в той же сети", cfg.Host)
	case strings.Contains(s, "unreachable") || strings.Contains(s, "no route"):
		return fmt.Errorf("нет маршрута до %s — ПК и колонка в разных сетях?", cfg.Host)
	case strings.Contains(s, "no such host") || strings.Contains(s, "lookup"):
		return fmt.Errorf("«%s» — это не IP-адрес и не имя, которое находится", cfg.Host)
	}
	return fmt.Errorf("не удалось подключиться к %s: %v", cfg.addr(), err)
}

func explainSSHErr(err error, cfg SSHConfig) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "unable to authenticate") || strings.Contains(s, "no supported methods remain"):
		return fmt.Errorf("неверный логин или пароль (%s)", cfg.User)
	case strings.Contains(s, "EOF") || strings.Contains(s, "reset") || strings.Contains(s, "forcibly closed"):
		return fmt.Errorf("колонка сбросила соединение на %d порту — это точно SSH?", cfg.Port)
	case strings.Contains(s, "no common algorithm") || strings.Contains(s, "handshake failed: ssh:"):
		return fmt.Errorf("не договорились о шифровании с SSH колонки: %v", err)
	}
	return fmt.Errorf("SSH: %v", err)
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Run выполняет команду; stdout и stderr объединяются. code = -1, если статус не получен.
func (r *Remote) Run(cmd string, stdin []byte, timeout time.Duration) (string, int, error) {
	if r.sys {
		return sysRun(r.cfg, cmd, stdin, timeout)
	}
	sess, err := r.cli.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("не удалось открыть сеанс: %v", err)
	}
	defer sess.Close()
	var out lockedBuf
	sess.Stdout, sess.Stderr = &out, &out
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	if err := sess.Start(cmd); err != nil {
		return "", -1, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err = <-done:
	case <-time.After(timeout):
		sess.Close()
		return out.String(), -1, fmt.Errorf("команда не завершилась за %d с", int(timeout.Seconds()))
	}
	if err == nil {
		return out.String(), 0, nil
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return out.String(), ee.ExitStatus(), nil
	}
	var em *ssh.ExitMissingError
	if errors.As(err, &em) {
		return out.String(), -1, nil
	}
	return out.String(), -1, err
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Upload кладёт файл на колонку через `cat > path`.
func (r *Remote) Upload(path string, data []byte, mode string) error {
	cmd := "cat > " + shQuote(path)
	if mode != "" {
		cmd += " && chmod " + mode + " " + shQuote(path)
	}
	out, code, err := r.Run(cmd, data, 30*time.Second)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("не удалось записать %s: %s", path, strings.TrimSpace(out))
	}
	return nil
}

// RunScript заливает скрипт во /tmp и запускает его через sh (так скрипт может читать stdin сам).
func (r *Remote) RunScript(name, body string, timeout time.Duration, args ...string) (string, int, error) {
	path := "/tmp/alisamic-" + name
	if err := r.Upload(path, []byte(body), "700"); err != nil {
		return "", -1, err
	}
	cmd := "sh " + shQuote(path)
	for _, a := range args {
		cmd += " " + shQuote(a)
	}
	cmd += " </dev/null 2>&1"
	out, code, err := r.Run(cmd, nil, timeout)
	_, _, _ = r.Run("rm -f "+shQuote(path), nil, 5*time.Second)
	return out, code, err
}

// ──────────────── ожидание перезагрузки ────────────────

func tcpAlive(host string, port int, to time.Duration) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), to)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// WaitReboot ждёт, пока SSH колонки пропадёт и появится снова.
func WaitReboot(cfg SSHConfig, log *opLog, maxWait time.Duration) bool {
	start := time.Now()
	log.add("жду, пока колонка выключится…")
	downSeen := false
	for time.Since(start) < 40*time.Second {
		if !tcpAlive(cfg.Host, cfg.Port, time.Second) {
			downSeen = true
			break
		}
		time.Sleep(time.Second)
	}
	if !downSeen {
		log.add("колонка не пропала из сети — возможно, перезагрузка не началась")
	} else {
		log.add("колонка выключилась, жду загрузку…")
	}
	last := 0
	for time.Since(start) < maxWait {
		if tcpAlive(cfg.Host, cfg.Port, 2*time.Second) {
			time.Sleep(3 * time.Second) // дать dropbear полностью подняться
			log.add("колонка снова в сети")
			return true
		}
		if s := int(time.Since(start).Seconds()) / 15; s != last {
			last = s
			log.add(fmt.Sprintf("всё ещё жду… %d с", int(time.Since(start).Seconds())))
		}
		time.Sleep(2 * time.Second)
	}
	return false
}
