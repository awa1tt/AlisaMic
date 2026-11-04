package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// disclaimerVersion — при изменении текста отказа от ответственности окно покажется снова.
const disclaimerVersion = "1"

// Settings — всё, что можно поменять в меню. Сохраняется в alisa-mic.json рядом с exe
// (если туда нельзя писать — в %APPDATA%\AlisaMic).
type Settings struct {
	Output    string  `json:"output"`    // имя устройства вывода ("" — по умолчанию)
	Gain      float64 `json:"gain"`      // предел автогейна (или фиксированное усиление, если АРУ выкл.)
	AGC       bool    `json:"agc"`       // автогейн
	Port      int     `json:"port"`      // UDP-порт, на который колонка шлёт звук
	BufferMs  int     `json:"buffer_ms"` // объём очереди вывода
	Autostart bool    `json:"autostart"` // запускать трансляцию сразу при открытии

	SSHHost  string `json:"ssh_host"`  // IP колонки
	SSHPort  int    `json:"ssh_port"`  // порт SSH (у рутованных станций обычно 2222)
	SSHUser  string `json:"ssh_user"`  // логин (обычно root)
	SSHPass  string `json:"ssh_pass"`  // пароль — сохраняется только при SavePass
	SavePass bool   `json:"save_pass"` // запоминать пароль в файле настроек (открытым текстом!)
	PCIP     string `json:"pc_ip"`     // IP этого ПК для колонки; пусто — определить автоматически

	Accepted string `json:"accepted"` // версия принятого отказа от ответственности
}

func defaultSettings() Settings {
	return Settings{Output: "CABLE Input", Gain: 80, AGC: true, Port: 5000, BufferMs: 480,
		SSHPort: 2222, SSHUser: "root"}
}

var (
	cfgMu       sync.Mutex
	cfgPathOnce string
)

// configPath — где лежит alisa-mic.json. Приоритет: уже существующий файл, затем папка exe,
// затем %APPDATA%\AlisaMic.
func configPath() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if cfgPathOnce != "" {
		return cfgPathOnce
	}
	cfgPathOnce = pickConfigPath("alisa-mic.json")
	return cfgPathOnce
}

func pickConfigPath(name string) string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), name))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		cands = append(cands, filepath.Join(dir, "AlisaMic", name))
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, p := range cands {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return p
		}
	}
	return name
}

func loadSettings() Settings {
	s := defaultSettings()
	p := configPath()
	b, err := os.ReadFile(p)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		b, err = os.ReadFile(p + ".bak")
	}
	if err == nil && len(strings.TrimSpace(string(b))) > 0 {
		_ = json.Unmarshal(b, &s)
	}
	s.clamp()
	return s
}

// writeFileAtomic пишет через временный файл, чтобы не оставить полупустой конфиг при сбое,
// и сохраняет резервную копию предыдущего рабочего конфига (*.bak).
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return os.WriteFile(path, data, 0o600)
	}
	return nil
}

func (s Settings) save() error {
	if !s.SavePass {
		s.SSHPass = ""
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeFileAtomic(configPath(), b)
}

func (s *Settings) clamp() {
	s.Gain = clampF(s.Gain, 1, 200)
	if s.Port < 1 || s.Port > 65535 {
		s.Port = 5000
	}
	if s.SSHPort < 1 || s.SSHPort > 65535 {
		s.SSHPort = 2222
	}
	s.SSHHost = strings.TrimSpace(s.SSHHost)
	s.SSHUser = strings.TrimSpace(s.SSHUser)
	if s.SSHUser == "" {
		s.SSHUser = "root"
	}
	s.PCIP = strings.TrimSpace(s.PCIP)
	s.BufferMs = int(clampF(float64(s.BufferMs), 120, 1000))
	s.BufferMs = s.BufferMs / chunkMs * chunkMs
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ───────────── доверенные ключи хоста (TOFU) ─────────────

var (
	hostsMu   sync.Mutex
	hostsData map[string]string
)

func hostsPath() string {
	return filepath.Join(filepath.Dir(configPath()), "alisa-mic-hosts.json")
}

func loadHostsLocked() {
	if hostsData != nil {
		return
	}
	hostsData = map[string]string{}
	if b, err := os.ReadFile(hostsPath()); err == nil {
		_ = json.Unmarshal(b, &hostsData)
	}
}

func knownHostKey(hostport string) string {
	hostsMu.Lock()
	defer hostsMu.Unlock()
	loadHostsLocked()
	return hostsData[hostport]
}

func rememberHostKey(hostport, fp string) {
	hostsMu.Lock()
	defer hostsMu.Unlock()
	loadHostsLocked()
	if fp == "" {
		delete(hostsData, hostport)
	} else {
		hostsData[hostport] = fp
	}
	b, _ := json.MarshalIndent(hostsData, "", "  ")
	_ = writeFileAtomic(hostsPath(), b)
}
