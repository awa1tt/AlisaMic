// Алиса · микрофон — превращает Яндекс Станцию в беспроводной микрофон для ПК.
// Принимает обработанный звук колонки (vqe_0, 16 кГц моно) по UDP
// и выводит его в виртуальный кабель VB-Cable или в колонки.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const version = "v1.0"

const repoURL = "github.com/awa1tt/AlisaMic"

func main() {
	if os.Getenv("ALISAMIC_ASKPASS") == "1" {
		askpassMain()
	}
	s := loadSettings()

	headless := flag.Bool("headless", false, "без меню: просто запустить трансляцию и писать статистику")
	out := flag.String("out", "", "устройство вывода (часть имени, \"-\" — колонки по умолчанию)")
	gain := flag.Float64("gain", 0, "предел усиления (1–200)")
	port := flag.Int("port", 0, "UDP-порт")
	buffer := flag.Int("buffer", 0, "буфер вывода, мс (120–1000)")
	noAGC := flag.Bool("no-agc", false, "выключить автогейн")
	flag.Parse()

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "out":
			s.Output = *out
			if *out == "-" {
				s.Output = ""
			}
		case "gain":
			s.Gain = *gain
		case "port":
			s.Port = *port
		case "buffer":
			s.BufferMs = *buffer
		case "no-agc":
			s.AGC = !*noAGC
		}
	})
	s.clamp()

	setupConsole()

	engine := NewEngine()
	if *headless {
		os.Exit(runHeadless(s, engine))
	}

	p := tea.NewProgram(newModel(s, engine), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка интерфейса:", err)
		engine.Stop()
		os.Exit(1)
	}
}

// runHeadless — режим без меню (для ярлыков и автозагрузки).
func runHeadless(s Settings, e *Engine) int {
	outs := OutputDevices()
	dev := outs[0]
	if s.Output != "" {
		i := FindDevice(outs, s.Output)
		if i < 0 {
			fmt.Printf("Устройство «%s» не найдено. Доступные:\n", s.Output)
			for _, d := range outs {
				fmt.Println("  ", d.Name)
			}
			return 1
		}
		dev = outs[i]
	}
	if err := e.Start(s, dev); err != nil {
		fmt.Println("Ошибка:", err)
		return 1
	}
	fmt.Printf("AlisaMic %s\n%s. Используйте на свой риск (меню → «Отказ от ответственности»).\nВывод: %s\nСлушаю UDP :%d (этот ПК: %s), усиление ×%g, автогейн %s\nCtrl+C — выход\n\n",
		version, testedOnly, dev.Name, s.Port, localIPFor(s.SSHHost), s.Gain, onOff(s.AGC))

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var lastP, lastB uint64
	for {
		select {
		case <-stop:
			e.Stop()
			return 0
		case <-t.C:
			p, b := e.Packets.Load(), e.Bytes.Load()
			state := "ждём колонку"
			if e.HasSignal() {
				state = "в эфире от " + e.Sender.Load().(string)
			}
			fmt.Printf("%s  %s  пак/с %d  байт/с %d  потерь %d  ×%.1f\n",
				time.Now().Format("15:04:05"), padTo(state, 28), (p-lastP)/5, (b-lastB)/5, e.Drops.Load(), e.CurGain.Load())
			lastP, lastB = p, b
		}
	}
}

func padTo(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
