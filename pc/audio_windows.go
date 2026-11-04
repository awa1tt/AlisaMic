package main

// Тонкая обёртка над winmm.dll: вывод (waveOut) и захват (waveIn),
// 16 кГц / 16 бит / моно. Без cgo — только syscall.

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	sampleRate    = 16000
	bytesPerMs    = sampleRate * 2 / 1000 // 32 байта на мс
	waveMapper    = 0xFFFFFFFF            // устройство по умолчанию
	callbackEvent = 0x00050000
	whdrDone      = 0x1
	whdrPrepared  = 0x2
)

var (
	winmm = windows.NewLazySystemDLL("winmm.dll")

	procWaveOutGetNumDevs      = winmm.NewProc("waveOutGetNumDevs")
	procWaveOutGetDevCapsW     = winmm.NewProc("waveOutGetDevCapsW")
	procWaveOutOpen            = winmm.NewProc("waveOutOpen")
	procWaveOutPrepareHeader   = winmm.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepareHeader = winmm.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite           = winmm.NewProc("waveOutWrite")
	procWaveOutReset           = winmm.NewProc("waveOutReset")
	procWaveOutClose           = winmm.NewProc("waveOutClose")
	procWaveInGetNumDevs       = winmm.NewProc("waveInGetNumDevs")
	procWaveInGetDevCapsW      = winmm.NewProc("waveInGetDevCapsW")
	procWaveInOpen             = winmm.NewProc("waveInOpen")
	procWaveInPrepareHeader    = winmm.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader  = winmm.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer        = winmm.NewProc("waveInAddBuffer")
	procWaveInStart            = winmm.NewProc("waveInStart")
	procWaveInReset            = winmm.NewProc("waveInReset")
	procWaveInClose            = winmm.NewProc("waveInClose")
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

type waveOutCaps struct {
	Mid, Pid      uint16
	DriverVersion uint32
	Pname         [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved1     uint16
	Support       uint32
}

type waveInCaps struct {
	Mid, Pid      uint16
	DriverVersion uint32
	Pname         [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved1     uint16
}

var pcmFormat = waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: sampleRate,
	AvgBytesPerSec: sampleRate * 2, BlockAlign: 2, BitsPerSample: 16}

// Device — звуковое устройство. ID = waveMapper означает «по умолчанию».
type Device struct {
	ID   uint32
	Name string
}

func (d Device) IsDefault() bool { return d.ID == waveMapper }

func mmErr(op string, r uintptr) error {
	if r == 0 {
		return nil
	}
	return fmt.Errorf("%s: ошибка winmm %d", op, r)
}

// OutputDevices — список устройств вывода; первым идёт «по умолчанию».
func OutputDevices() []Device {
	list := []Device{{ID: waveMapper, Name: "Колонки по умолчанию"}}
	n, _, _ := procWaveOutGetNumDevs.Call()
	for i := uintptr(0); i < n; i++ {
		var c waveOutCaps
		if r, _, _ := procWaveOutGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c)); r == 0 {
			list = append(list, Device{ID: uint32(i), Name: strings.TrimSpace(windows.UTF16ToString(c.Pname[:]))})
		}
	}
	return list
}

// InputDevices — список устройств записи.
func InputDevices() []Device {
	var list []Device
	n, _, _ := procWaveInGetNumDevs.Call()
	for i := uintptr(0); i < n; i++ {
		var c waveInCaps
		if r, _, _ := procWaveInGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c)); r == 0 {
			list = append(list, Device{ID: uint32(i), Name: strings.TrimSpace(windows.UTF16ToString(c.Pname[:]))})
		}
	}
	return list
}

// FindDevice ищет устройство по имени: сначала точное совпадение, потом по подстроке.
func FindDevice(list []Device, name string) int {
	if name == "" {
		return -1
	}
	for i, d := range list {
		if strings.EqualFold(d.Name, name) {
			return i
		}
	}
	low := strings.ToLower(name)
	for i, d := range list {
		if strings.Contains(strings.ToLower(d.Name), low) {
			return i
		}
	}
	return -1
}

// ringBuffers — заголовки и данные, закреплённые в памяти (драйвер пишет в них асинхронно).
type ringBuffers struct {
	hdrs []waveHdr
	data []byte
	size int
	pin  runtime.Pinner
}

func newRing(n, size int) *ringBuffers {
	r := &ringBuffers{hdrs: make([]waveHdr, n), data: make([]byte, n*size), size: size}
	r.pin.Pin(&r.hdrs[0])
	r.pin.Pin(&r.data[0])
	for i := range r.hdrs {
		r.hdrs[i].Data = uintptr(unsafe.Pointer(&r.data[i*size]))
		r.hdrs[i].BufferLength = uint32(size)
	}
	return r
}

func (r *ringBuffers) flags(i int) uint32 { return atomic.LoadUint32(&r.hdrs[i].Flags) }
func (r *ringBuffers) slot(i int) []byte  { return r.data[i*r.size : (i+1)*r.size] }
func (r *ringBuffers) hdr(i int) uintptr  { return uintptr(unsafe.Pointer(&r.hdrs[i])) }
func (r *ringBuffers) free()              { r.pin.Unpin() }

var hdrSize = unsafe.Sizeof(waveHdr{})

// ───────────────────────────── вывод ─────────────────────────────

type Player struct {
	h    uintptr
	evt  windows.Handle
	ring *ringBuffers
}

func OpenPlayer(dev uint32, buffers, chunk int) (*Player, error) {
	evt, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	p := &Player{evt: evt}
	r, _, _ := procWaveOutOpen.Call(uintptr(unsafe.Pointer(&p.h)), uintptr(dev),
		uintptr(unsafe.Pointer(&pcmFormat)), uintptr(evt), 0, callbackEvent)
	if r != 0 {
		windows.CloseHandle(evt)
		return nil, fmt.Errorf("не удалось открыть устройство вывода (код %d)", r)
	}
	p.ring = newRing(buffers, chunk)
	for i := range p.ring.hdrs {
		p.ring.hdrs[i].Flags = whdrDone // свободен
	}
	return p, nil
}

// Write отправляет кусок в драйвер. false — свободного буфера не нашлось за timeout (кусок выброшен).
func (p *Player) Write(chunk []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		for i := range p.ring.hdrs {
			f := p.ring.flags(i)
			if f&whdrDone == 0 {
				continue
			}
			if f&whdrPrepared != 0 {
				procWaveOutUnprepareHeader.Call(p.h, p.ring.hdr(i), hdrSize)
			}
			n := copy(p.ring.slot(i), chunk)
			p.ring.hdrs[i].BufferLength = uint32(n)
			atomic.StoreUint32(&p.ring.hdrs[i].Flags, 0)
			procWaveOutPrepareHeader.Call(p.h, p.ring.hdr(i), hdrSize)
			procWaveOutWrite.Call(p.h, p.ring.hdr(i), hdrSize)
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		windows.WaitForSingleObject(p.evt, uint32(left/time.Millisecond)+1)
	}
}

// Queued — сколько буферов сейчас в очереди драйвера.
func (p *Player) Queued() int {
	q := 0
	for i := range p.ring.hdrs {
		if p.ring.flags(i)&whdrDone == 0 {
			q++
		}
	}
	return q
}

func (p *Player) Close() {
	procWaveOutReset.Call(p.h)
	for i := range p.ring.hdrs {
		if p.ring.flags(i)&whdrPrepared != 0 {
			procWaveOutUnprepareHeader.Call(p.h, p.ring.hdr(i), hdrSize)
		}
	}
	procWaveOutClose.Call(p.h)
	windows.CloseHandle(p.evt)
	p.ring.free()
}

// ───────────────────────────── захват ─────────────────────────────

// Recorder непрерывно пишет с устройства и отдаёт уровень каждого блока в onBlock.
type Recorder struct {
	h    uintptr
	evt  windows.Handle
	ring *ringBuffers
	stop chan struct{}
	done chan struct{}
}

func StartRecorder(dev uint32, onBlock func(pcm []byte)) (*Recorder, error) {
	evt, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	rec := &Recorder{evt: evt, stop: make(chan struct{}), done: make(chan struct{})}
	r, _, _ := procWaveInOpen.Call(uintptr(unsafe.Pointer(&rec.h)), uintptr(dev),
		uintptr(unsafe.Pointer(&pcmFormat)), uintptr(evt), 0, callbackEvent)
	if r != 0 {
		windows.CloseHandle(evt)
		return nil, fmt.Errorf("не удалось открыть устройство записи (код %d)", r)
	}
	rec.ring = newRing(4, 100*bytesPerMs) // 4 × 100 мс
	for i := range rec.ring.hdrs {
		procWaveInPrepareHeader.Call(rec.h, rec.ring.hdr(i), hdrSize)
		procWaveInAddBuffer.Call(rec.h, rec.ring.hdr(i), hdrSize)
	}
	if err := mmErr("waveInStart", func() uintptr { r, _, _ := procWaveInStart.Call(rec.h); return r }()); err != nil {
		rec.closeDevice()
		return nil, err
	}
	go rec.loop(onBlock)
	return rec, nil
}

func (rec *Recorder) loop(onBlock func([]byte)) {
	defer close(rec.done)
	for {
		select {
		case <-rec.stop:
			return
		default:
		}
		windows.WaitForSingleObject(rec.evt, 100)
		for i := range rec.ring.hdrs {
			if rec.ring.flags(i)&whdrDone == 0 {
				continue
			}
			n := int(rec.ring.hdrs[i].BytesRecorded)
			if n > 0 {
				onBlock(rec.ring.slot(i)[:n])
			}
			procWaveInUnprepareHeader.Call(rec.h, rec.ring.hdr(i), hdrSize)
			rec.ring.hdrs[i].BufferLength = uint32(rec.ring.size)
			rec.ring.hdrs[i].BytesRecorded = 0
			atomic.StoreUint32(&rec.ring.hdrs[i].Flags, 0)
			procWaveInPrepareHeader.Call(rec.h, rec.ring.hdr(i), hdrSize)
			procWaveInAddBuffer.Call(rec.h, rec.ring.hdr(i), hdrSize)
		}
	}
}

func (rec *Recorder) closeDevice() {
	procWaveInReset.Call(rec.h)
	for i := range rec.ring.hdrs {
		if rec.ring.flags(i)&whdrPrepared != 0 {
			procWaveInUnprepareHeader.Call(rec.h, rec.ring.hdr(i), hdrSize)
		}
	}
	procWaveInClose.Call(rec.h)
	windows.CloseHandle(rec.evt)
	rec.ring.free()
}

func (rec *Recorder) Stop() {
	close(rec.stop)
	<-rec.done
	rec.closeDevice()
}
