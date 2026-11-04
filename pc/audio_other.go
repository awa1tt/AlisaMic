//go:build !windows

package main

// Заглушка для сборки и проверки интерфейса вне Windows (звук не воспроизводится).

import (
	"strings"
	"time"
)

const (
	sampleRate = 16000
	bytesPerMs = sampleRate * 2 / 1000
	waveMapper = 0xFFFFFFFF
)

type Device struct {
	ID   uint32
	Name string
}

func (d Device) IsDefault() bool { return d.ID == waveMapper }

func OutputDevices() []Device {
	return []Device{{ID: waveMapper, Name: "Колонки по умолчанию"}, {ID: 0, Name: "CABLE Input (VB-Audio Virtual C"}}
}
func InputDevices() []Device {
	return []Device{{ID: 0, Name: "Микрофон (тест)"}, {ID: 1, Name: "CABLE Output (VB-Audio Virtual "}}
}

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

type Player struct{ q int }

func OpenPlayer(dev uint32, buffers, chunk int) (*Player, error) { return &Player{}, nil }
func (p *Player) Write(chunk []byte, timeout time.Duration) bool {
	time.Sleep(35 * time.Millisecond)
	return true
}
func (p *Player) Queued() int { return 2 }
func (p *Player) Close()      {}

type Recorder struct{ stop chan struct{} }

func StartRecorder(dev uint32, onBlock func(pcm []byte)) (*Recorder, error) {
	r := &Recorder{stop: make(chan struct{})}
	go func() {
		blk := make([]byte, 3200)
		for i := range blk {
			blk[i] = byte(i * 7)
		}
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				onBlock(blk)
			}
		}
	}()
	return r, nil
}
func (r *Recorder) Stop() { close(r.stop) }
