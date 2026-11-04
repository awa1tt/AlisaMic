package main

// Мост: UDP от колонки → АРУ → устройство вывода.

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	chunkMs    = 40
	chunkBytes = chunkMs * bytesPerMs // 1280 байт
	agcTarget  = 8000.0               // целевой RMS речи
	clipLimit  = 32000
)

type atomicFloat struct{ v atomic.Uint64 }

func (a *atomicFloat) Load() float64   { return math.Float64frombits(a.v.Load()) }
func (a *atomicFloat) Store(f float64) { a.v.Store(math.Float64bits(f)) }

type Engine struct {
	mu   sync.Mutex
	conn *net.UDPConn
	done chan struct{}

	maxGain atomicFloat
	agcOn   atomic.Bool

	Packets  atomic.Uint64
	Bytes    atomic.Uint64
	Drops    atomic.Uint64
	LastPkt  atomic.Int64 // unix nano
	Sender   atomic.Value // string
	InLevel  atomicFloat  // RMS до усиления
	OutLevel atomicFloat  // RMS после усиления
	CurGain  atomicFloat
	QueuedMs atomic.Int64
}

func NewEngine() *Engine {
	e := &Engine{}
	e.Sender.Store("")
	return e
}

func (e *Engine) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conn != nil
}

func (e *Engine) SetGain(g float64) { e.maxGain.Store(g) }
func (e *Engine) SetAGC(on bool)    { e.agcOn.Store(on) }

// HasSignal — пакеты от колонки приходили за последние 1.5 с.
func (e *Engine) HasSignal() bool {
	t := e.LastPkt.Load()
	return t != 0 && time.Since(time.Unix(0, t)) < 1500*time.Millisecond
}

func (e *Engine) Start(s Settings, out Device) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn != nil {
		return nil
	}
	e.SetGain(s.Gain)
	e.SetAGC(s.AGC)

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: s.Port})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "permitted") || strings.Contains(strings.ToLower(err.Error()), "denied") {
			return fmt.Errorf("нет доступа к порту UDP %d: %v", s.Port, err)
		}
		return fmt.Errorf("порт UDP %d занят другой программой (возможно, второй AlisaMic)", s.Port)
	}
	_ = conn.SetReadBuffer(1 << 20)

	buffers := s.BufferMs / chunkMs
	pl, err := OpenPlayer(out.ID, buffers, chunkBytes)
	if err != nil {
		conn.Close()
		return fmt.Errorf("%s: %w", out.Name, err)
	}

	e.Packets.Store(0)
	e.Bytes.Store(0)
	e.Drops.Store(0)
	e.LastPkt.Store(0)
	e.InLevel.Store(0)
	e.OutLevel.Store(0)
	e.CurGain.Store(1)
	e.Sender.Store("")

	e.conn = conn
	e.done = make(chan struct{})
	go e.loop(conn, pl, e.done)
	return nil
}

func (e *Engine) Stop() {
	e.mu.Lock()
	conn, done := e.conn, e.done
	e.conn = nil
	e.mu.Unlock()
	if conn == nil {
		return
	}
	conn.Close()
	<-done
	e.InLevel.Store(0)
	e.OutLevel.Store(0)
}

func (e *Engine) loop(conn *net.UDPConn, pl *Player, done chan struct{}) {
	defer close(done)
	defer pl.Close()

	pkt := make([]byte, 4096)
	chunk := make([]byte, chunkBytes)
	filled := 0
	agc := 10.0

	for {
		n, addr, err := conn.ReadFromUDP(pkt)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		e.Packets.Add(1)
		e.Bytes.Add(uint64(n))
		e.LastPkt.Store(time.Now().UnixNano())
		if s := addr.IP.String(); s != e.Sender.Load().(string) {
			e.Sender.Store(s)
		}

		for off := 0; off < n; {
			take := copy(chunk[filled:], pkt[off:n])
			filled += take
			off += take
			if filled < chunkBytes {
				continue
			}
			filled = 0
			agc = e.process(chunk, agc)
			if !pl.Write(chunk, 50*time.Millisecond) {
				e.Drops.Add(1)
			}
			e.QueuedMs.Store(int64(pl.Queued() * chunkMs))
		}
	}
}

// process — АРУ: тихую речь вытягиваем до agcTarget, тишину не усиливаем.
func (e *Engine) process(chunk []byte, agc float64) float64 {
	samples := len(chunk) / 2
	var sum float64
	for i := 0; i < len(chunk); i += 2 {
		v := float64(int16(uint16(chunk[i]) | uint16(chunk[i+1])<<8))
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(samples))

	limit := e.maxGain.Load()
	if e.agcOn.Load() {
		if rms > 30 {
			agc += (agcTarget/rms - agc) * 0.5
		} else {
			agc += (1.5 - agc) * 0.05
		}
		agc = clampF(agc, 1, limit)
	} else {
		agc = limit
	}

	sum = 0
	for i := 0; i < len(chunk); i += 2 {
		v := float64(int16(uint16(chunk[i])|uint16(chunk[i+1])<<8)) * agc
		v = clampF(v, -clipLimit, clipLimit)
		s := int16(v)
		chunk[i], chunk[i+1] = byte(s), byte(uint16(s)>>8)
		sum += v * v
	}
	e.InLevel.Store(rms)
	e.OutLevel.Store(math.Sqrt(sum / float64(samples)))
	e.CurGain.Store(agc)
	return agc
}

// rmsOf — уровень произвольного блока s16le.
func rmsOf(pcm []byte) (rms float64, peak int) {
	var sum float64
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		sum += float64(v * v)
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	if len(pcm) < 2 {
		return 0, 0
	}
	return math.Sqrt(sum / float64(len(pcm)/2)), peak
}

// localIPFor — IP этого ПК в сети, из которой достижим target (пакеты при этом не отправляются).
// Если target пуст или недостижим — обычный маршрут по умолчанию.
func localIPFor(target string) string {
	for _, t := range []string{target, "192.168.0.1", "10.0.0.1"} {
		if t == "" {
			continue
		}
		c, err := net.Dial("udp4", net.JoinHostPort(t, "9"))
		if err != nil {
			continue
		}
		ip := c.LocalAddr().(*net.UDPAddr).IP.String()
		c.Close()
		if ip != "" && ip != "0.0.0.0" {
			return ip
		}
	}
	return "?"
}

// probeUDP слушает порт d времени и возвращает число пакетов и IP отправителя.
// Нужен диагностике, когда трансляция выключена.
func probeUDP(port int, d time.Duration) (packets int, sender string, err error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return 0, "", err
	}
	defer conn.Close()
	buf := make([]byte, 4096)
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		_ = conn.SetReadDeadline(end)
		_, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		packets++
		sender = addr.IP.String()
	}
	return packets, sender, nil
}

// portFree — можно ли занять UDP-порт.
func portFree(port int) bool {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return false
	}
	c.Close()
	return true
}
