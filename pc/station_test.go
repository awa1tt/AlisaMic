//go:build !windows

package main

// Тест на «фейковой колонке»: настоящий SSH-сервер в процессе теста выполняет скрипты
// в изолированной папке (пути /system, /data, /etc переписываются). Проверяет логику
// установки, обновления адреса, отката, смены пароля и SSH-порта.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GehirnInc/crypt/sha512_crypt"
	"golang.org/x/crypto/ssh"
)

type fakeAlice struct {
	t         *testing.T
	root      string
	mu        sync.Mutex
	lns       map[int]net.Listener
	hk        ssh.Signer
	useShadow bool // true: пароль проверяется по /etc/shadow, false: фиксированный
	fixedPw   string
	procs     []*exec.Cmd
}

var rewrites = []string{"/system/vendor", "/data/quasar", "/data/opt", "/etc/init.d", "/etc/shadow", "/etc/passwd"}

func (f *fakeAlice) rw(s string) string {
	for _, p := range rewrites {
		s = strings.ReplaceAll(s, p, f.root+p)
	}
	return s
}

const launchTmpl = `{
  "services": [
    {
      "name": "maind",
      "cmd": "/system/bin/maind",
      "env": [
                        "LANG=C",
                        "CA_CERTIFICATES=/system/etc/ca.pem"%s
      ]
    },
    {
      "name": "other",
      "env": ["X=1"]
    }
  ]
}
`

func newFakeAlice(t *testing.T, trailingComma bool) *fakeAlice {
	f := &fakeAlice{t: t, root: t.TempDir(), lns: map[int]net.Listener{}, useShadow: true}
	for _, d := range []string{"system/vendor/quasar/libs", "data/quasar", "data/opt/etc/dropbear", "etc/init.d", "bin", "shim"} {
		os.MkdirAll(filepath.Join(f.root, d), 0o755)
	}
	extra := ""
	if trailingComma {
		extra = ",\n                        \"TZ=UTC\""
		// запятая после CA_CERTIFICATES, дальше ещё элемент
	}
	launch := fmt.Sprintf(launchTmpl, extra)
	os.WriteFile(filepath.Join(f.root, "system/vendor/quasar/launch.json"), []byte(launch), 0o644)
	os.WriteFile(filepath.Join(f.root, "system/vendor/quasar/libs/libyandex_evil.so"), []byte("xx YandexVqeC_GetOutputChannelData xx"), 0o644)
	os.WriteFile(filepath.Join(f.root, "etc/init.d/S99zdropbear"), []byte("#!/bin/sh\nDB=/data/opt/bin/dropbear\n\"$DB\" -r \"$HK\" -p 2222 -B 2>>\"$LOG\"\n"), 0o755)
	os.WriteFile(filepath.Join(f.root, "etc/shadow"), []byte("root::19000:0:99999:7:::\ndaemon:*:19000:0:99999:7:::\n"), 0o600)
	for _, n := range []string{"mount", "sync", "reboot"} {
		os.WriteFile(filepath.Join(f.root, "shim", n), []byte("#!/bin/sh\n[ \""+n+"\" = reboot ] && touch "+f.root+"/rebooted\nexit 0\n"), 0o755)
	}
	// фейковый «процесс» с нужным именем: спит и держит cmdline
	src := filepath.Join(f.root, "fake.go")
	os.WriteFile(src, []byte("package main\nimport \"time\"\nfunc main(){ time.Sleep(time.Hour) }\n"), 0o644)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("нет go в PATH — не могу собрать фейковые процессы")
	}
	for _, n := range []string{"dropbear", "maind"} {
		out, err := exec.Command(goBin, "build", "-o", filepath.Join(f.root, "bin", n), src).CombinedOutput()
		if err != nil {
			t.Fatalf("сборка фейка: %v %s", err, out)
		}
	}
	f.startFake("dropbear", "-r", "/x/hk", "-p", "2222", "-B")
	f.startFake("maind", "--platformd")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	f.hk, _ = ssh.NewSignerFromKey(priv)
	t.Cleanup(f.close)
	return f
}

func (f *fakeAlice) startFake(name string, args ...string) {
	c := exec.Command(filepath.Join(f.root, "bin", name), args...)
	if err := c.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.procs = append(f.procs, c)
}

func (f *fakeAlice) close() {
	f.mu.Lock()
	for _, l := range f.lns {
		l.Close()
	}
	f.mu.Unlock()
	for _, c := range f.procs {
		c.Process.Kill()
		c.Wait()
	}
	exec.Command("pkill", "-f", f.root).Run()
}

func (f *fakeAlice) checkPassword(user, pw string) bool {
	if !f.useShadow {
		return pw == f.fixedPw
	}
	b, _ := os.ReadFile(filepath.Join(f.root, "etc/shadow"))
	for _, ln := range strings.Split(string(b), "\n") {
		p := strings.Split(ln, ":")
		if len(p) > 1 && p[0] == user {
			if p[1] == "" {
				return pw == ""
			}
			return sha512_crypt.New().Verify(p[1], []byte(pw)) == nil
		}
	}
	return false
}

func (f *fakeAlice) listen(port int) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		f.t.Fatalf("listen %d: %v", port, err)
	}
	f.mu.Lock()
	f.lns[port] = ln
	f.mu.Unlock()
	conf := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if f.checkPassword(c.User(), string(pw)) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	conf.AddHostKey(f.hk)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(nc, conf)
		}
	}()
}

func (f *fakeAlice) serve(nc net.Conn, conf *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, conf)
	if err != nil {
		nc.Close()
		return
	}
	go func() {
		for r := range reqs {
			if r.WantReply {
				r.Reply(false, nil)
			}
		}
	}()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, _ := nch.Accept()
		go func() {
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(r.Payload)
				cmd := string(r.Payload[4 : 4+n])
				r.Reply(true, nil)
				go f.exec(ch, cmd)
			}
		}()
	}
	sc.Close()
}

func (f *fakeAlice) exec(ch ssh.Channel, cmd string) {
	defer ch.Close()
	in, _ := io.ReadAll(io.LimitReader(ch, 1<<20)) // клиент закрывает stdin после данных
	if strings.HasPrefix(cmd, "cat >") && !strings.Contains(cmd, ".so") {
		in = []byte(f.rw(string(in)))
	}
	c := exec.Command("sh", "-c", f.rw(cmd))
	c.Env = append(os.Environ(), "PATH="+filepath.Join(f.root, "shim")+":"+os.Getenv("PATH"))
	c.Stdin = strings.NewReader(string(in))
	c.Stdout, c.Stderr = ch, ch
	err := c.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	// после нужных команд — поведение «железа»
	if strings.Contains(cmd, "alisamic-sshport.sh") {
		f.afterSSHPort(cmd)
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{uint32(code)}))
}

func (f *fakeAlice) afterSSHPort(cmd string) {
	fields := strings.Fields(strings.ReplaceAll(cmd, "'", ""))
	for i, w := range fields {
		if strings.HasSuffix(w, "alisamic-sshport.sh") && i+2 < len(fields) {
			act, port := fields[i+1], 0
			fmt.Sscan(fields[i+2], &port)
			switch act {
			case "start":
				if port > 0 && !f.useShadowFlagNoNew() {
					f.listen(port)
				}
			case "finish", "rollback":
				f.mu.Lock()
				if l := f.lns[port]; l != nil {
					l.Close()
					delete(f.lns, port)
				}
				f.mu.Unlock()
			}
		}
	}
}

func (f *fakeAlice) useShadowFlagNoNew() bool { return os.Getenv("FAKE_NO_NEW_PORT") == "1" }

func (f *fakeAlice) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(f.root, rel))
	return string(b)
}

func freeTCPPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (f *fakeAlice) cfg(port int, pw string) SSHConfig {
	return SSHConfig{Host: "127.0.0.1", Port: port, User: "root", Password: pw}
}

func logOf(l *opLog) string { return strings.Join(l.snapshot(), "\n") }

func TestInstallFlow(t *testing.T) {
	for _, comma := range []bool{false, true} {
		t.Run(fmt.Sprintf("comma=%v", comma), func(t *testing.T) {
			f := newFakeAlice(t, comma)
			port := freeTCPPort(t)
			f.listen(port)
			cfg := f.cfg(port, "")
			orig := f.read("system/vendor/quasar/launch.json")

			st, r, err := FetchStatus(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			if !st.b("vqe_symbol") || st.b("so") || st.b("patched") || !st.b("maind") {
				t.Fatalf("исходный статус неверен: %v", st.kv)
			}
			if !strings.Contains(st.s("ssh_cmd"), "dropbear") {
				t.Fatalf("не вижу dropbear: %v", st.kv)
			}

			l := &opLog{}
			if err := InstallOnStation(cfg, "192.168.3.5", 5000, l); err != nil {
				t.Fatalf("установка: %v\n%s", err, logOf(l))
			}
			lj := f.read("system/vendor/quasar/launch.json")
			var js any
			if err := json.Unmarshal([]byte(lj), &js); err != nil {
				t.Fatalf("launch.json после патча невалиден: %v\n%s", err, lj)
			}
			if strings.Count(lj, "LD_PRELOAD") != 1 || !strings.Contains(lj, "ALICE_TAP_TARGET=192.168.3.5:5000") {
				t.Fatalf("патч не тот:\n%s", lj)
			}
			if f.read("system/vendor/quasar/launch.json.bak-alisamic") != orig {
				t.Fatal("бэкап не равен оригиналу")
			}
			if string(tapSO) != f.read("data/quasar/micstream/libvqe_tap.so") {
				t.Fatal("libvqe_tap.so повреждён")
			}

			// повторная установка с другим адресом
			l = &opLog{}
			if err := InstallOnStation(cfg, "10.0.0.7", 5001, l); err != nil {
				t.Fatalf("повторная установка: %v\n%s", err, logOf(l))
			}
			lj = f.read("system/vendor/quasar/launch.json")
			if strings.Count(lj, "LD_PRELOAD") != 1 || !strings.Contains(lj, "ALICE_TAP_TARGET=10.0.0.7:5001") || strings.Contains(lj, "192.168.3.5") {
				t.Fatalf("адрес не обновился:\n%s", lj)
			}
			if f.read("system/vendor/quasar/launch.json.bak-alisamic") != orig {
				t.Fatal("бэкап перезаписан")
			}

			st, r, err = FetchStatus(cfg)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			if !st.Installed() || !st.UpToDate() || st.s("target") != "10.0.0.7:5001" {
				t.Fatalf("статус после установки: %v", st.kv)
			}

			// откат
			l = &opLog{}
			if err := UninstallFromStation(cfg, l); err != nil {
				t.Fatalf("откат: %v\n%s", err, logOf(l))
			}
			if f.read("system/vendor/quasar/launch.json") != orig {
				t.Fatal("launch.json не вернулся")
			}
			if _, err := os.Stat(filepath.Join(f.root, "data/quasar/micstream/libvqe_tap.so")); err == nil {
				t.Fatal(".so не удалён")
			}
			t.Log(logOf(l))
		})
	}
}

func TestForeignPreloadIsNotTouched(t *testing.T) {
	f := newFakeAlice(t, false)
	port := freeTCPPort(t)
	f.listen(port)
	p := filepath.Join(f.root, "system/vendor/quasar/launch.json")
	b, _ := os.ReadFile(p)
	foreign := strings.Replace(string(b), `"LANG=C"`, `"LANG=C", "LD_PRELOAD=/data/other.so"`, 1)
	os.WriteFile(p, []byte(foreign), 0o644)
	l := &opLog{}
	err := InstallOnStation(f.cfg(port, ""), "1.2.3.4", 5000, l)
	if err == nil || !strings.Contains(err.Error(), "чужой LD_PRELOAD") {
		t.Fatalf("ожидал отказ из-за чужого LD_PRELOAD, получил %v\n%s", err, logOf(l))
	}
	if f.read("system/vendor/quasar/launch.json") != foreign {
		t.Fatal("launch.json изменён")
	}
}

func TestPasswordChange(t *testing.T) {
	f := newFakeAlice(t, false)
	port := freeTCPPort(t)
	f.listen(port)
	cfg := f.cfg(port, "")
	l := &opLog{}
	if err := ChangeSSHPassword(cfg, "Секрет 123$'\"", l); err != nil {
		t.Fatalf("%v\n%s", err, logOf(l))
	}
	if !f.checkPassword("root", "Секрет 123$'\"") || f.checkPassword("root", "") {
		t.Fatal("пароль на колонке не тот")
	}
	if !strings.Contains(f.read("etc/shadow"), "daemon:*:") {
		t.Fatal("другие строки shadow потеряны")
	}
	// дальше вход работает только с новым
	if _, err := DialSSH(cfg, 3*time.Second); err == nil {
		t.Fatal("старый пароль всё ещё принимается")
	}
	cfg.Password = "Секрет 123$'\""
	r, err := DialSSH(cfg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

func TestPasswordRollbackWhenServerIgnoresShadow(t *testing.T) {
	f := newFakeAlice(t, false)
	f.useShadow = false
	f.fixedPw = "old"
	port := freeTCPPort(t)
	f.listen(port)
	orig := f.read("etc/shadow")
	l := &opLog{}
	err := ChangeSSHPassword(f.cfg(port, "old"), "new", l)
	if err == nil || !strings.Contains(err.Error(), "восстановлен") {
		t.Fatalf("ожидал откат, получил %v\n%s", err, logOf(l))
	}
	if f.read("etc/shadow") != orig {
		t.Fatal("shadow не восстановлен")
	}
}

func TestSSHPortChange(t *testing.T) {
	f := newFakeAlice(t, false)
	oldPort := freeTCPPort(t)
	// фейковый dropbear должен «слушать» тот же порт, что и в cmdline — подменим его
	exec.Command("pkill", "-f", f.root+"/bin/dropbear").Run()
	f.startFake("dropbear", "-r", "/x/hk", "-p", strconv.Itoa(oldPort), "-B")
	os.WriteFile(filepath.Join(f.root, "etc/init.d/S99zdropbear"), []byte(fmt.Sprintf("#!/bin/sh\nDB=/data/opt/bin/dropbear\n\"$DB\" -r \"$HK\" -p %d -B 2>>\"$LOG\"\n", oldPort)), 0o755)
	f.listen(oldPort)
	time.Sleep(300 * time.Millisecond)
	newPort := freeTCPPort(t)
	cfg := f.cfg(oldPort, "")

	l := &opLog{}
	if err := ChangeSSHPort(cfg, newPort, l); err != nil {
		t.Fatalf("%v\n%s", err, logOf(l))
	}
	if !strings.Contains(f.read("etc/init.d/S99zdropbear"), fmt.Sprintf("-p %d -B", newPort)) {
		t.Fatalf("init-скрипт не обновлён:\n%s", f.read("etc/init.d/S99zdropbear"))
	}
	if _, err := os.Stat(filepath.Join(f.root, "etc/init.d/S99zdropbear.bak-alisamic")); err != nil {
		t.Fatal("нет бэкапа init-скрипта")
	}
	// старый слушатель закрыт, новый жив
	if tcpAlive("127.0.0.1", oldPort, 300*time.Millisecond) {
		t.Fatal("старый порт ещё открыт")
	}
	ncfg := cfg
	ncfg.Port = newPort
	r, err := DialSSH(ncfg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	t.Log(logOf(l))
}

func TestSSHPortRollbackWhenNewPortDead(t *testing.T) {
	os.Setenv("FAKE_NO_NEW_PORT", "1")
	defer os.Unsetenv("FAKE_NO_NEW_PORT")
	f := newFakeAlice(t, false)
	oldPort := freeTCPPort(t)
	exec.Command("pkill", "-f", f.root+"/bin/dropbear").Run()
	f.startFake("dropbear", "-r", "/x/hk", "-p", strconv.Itoa(oldPort), "-B")
	initP := filepath.Join(f.root, "etc/init.d/S99zdropbear")
	body := fmt.Sprintf("#!/bin/sh\nDB=/data/opt/bin/dropbear\n\"$DB\" -r \"$HK\" -p %d -B 2>>\"$LOG\"\n", oldPort)
	os.WriteFile(initP, []byte(body), 0o755)
	f.listen(oldPort)
	time.Sleep(300 * time.Millisecond)
	l := &opLog{}
	err := ChangeSSHPort(f.cfg(oldPort, ""), freeTCPPort(t), l)
	if err == nil || !strings.Contains(err.Error(), "откатил") {
		t.Fatalf("ожидал откат, получил %v\n%s", err, logOf(l))
	}
	if f.read("etc/init.d/S99zdropbear") != body {
		t.Fatalf("init-скрипт не восстановлен:\n%s", f.read("etc/init.d/S99zdropbear"))
	}
	if !tcpAlive("127.0.0.1", oldPort, 300*time.Millisecond) {
		t.Fatal("старый порт должен остаться открытым")
	}
}

func TestHostKeyTOFU(t *testing.T) {
	f := newFakeAlice(t, false)
	port := freeTCPPort(t)
	f.listen(port)
	cfg := f.cfg(port, "")
	rememberHostKey(cfg.addr(), "")
	r, err := DialSSH(cfg, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	rememberHostKey(cfg.addr(), "SHA256:WRONG")
	_, err = DialSSH(cfg, 3*time.Second)
	if _, ok := err.(*errHostKeyChanged); !ok {
		t.Fatalf("ожидал errHostKeyChanged, получил %v", err)
	}
	rememberHostKey(cfg.addr(), "")
}

func TestHashMatchesOpenSSL(t *testing.T) {
	h, err := sha512Hash("тест")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$6$") {
		t.Fatal(h)
	}
	if err := sha512_crypt.New().Verify(h, []byte("тест")); err != nil {
		t.Fatal(err)
	}
	// сверка с openssl, если он есть
	parts := strings.Split(h, "$")
	if out, err := exec.Command("openssl", "passwd", "-6", "-salt", parts[2], "тест").Output(); err == nil {
		if strings.TrimSpace(string(out)) != h {
			t.Fatalf("openssl дал другой хеш: %s vs %s", out, h)
		}
	}
}

// Ручной стенд: FAKE_SERVE=1 go test -run TestServeFakeForUI — поднимает фейковую колонку на 127.0.0.1:22222
func TestServeFakeForUI(t *testing.T) {
	if os.Getenv("FAKE_SERVE") != "1" {
		t.Skip()
	}
	f := newFakeAlice(t, false)
	f.listen(22222)
	os.WriteFile("/tmp/fake-root-path", []byte(f.root), 0o644)
	fmt.Println("FAKE ALICE READY", f.root)
	time.Sleep(15 * time.Minute)
}
