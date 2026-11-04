package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type COORD struct {
	X int16
	Y int16
}

type SMALL_RECT struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type CONSOLE_SCREEN_BUFFER_INFO struct {
	Size              COORD
	CursorPosition    COORD
	Attributes        uint16
	Window            SMALL_RECT
	MaximumWindowSize COORD
}

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleScreenBufferSize = kernel32.NewProc("SetConsoleScreenBufferSize")
	procSetConsoleWindowInfo       = kernel32.NewProc("SetConsoleWindowInfo")
)

// setupConsole — UTF-8 кодировка и автоматическая подгонка размера окна консоли.
func setupConsole() {
	_ = windows.SetConsoleOutputCP(65001)
	_ = windows.SetConsoleCP(65001)

	// Автоматически расширяем окно консоли (минимум 90x36)
	resizeConsole(90, 36)
}

func resizeConsole(minCols, minRows int16) {
	// 1. Посылаем ANSI VT-код изменения размера окна (для Windows Terminal, VSCode, ConEmu)
	fmt.Fprintf(os.Stdout, "\x1b[8;%d;%dt", minRows, minCols)

	// 2. Win32 API для классической консоли Windows (conhost.exe)
	h, err := syscall.CreateFile(syscall.StringToUTF16Ptr("CONOUT$"),
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)

	var csbi CONSOLE_SCREEN_BUFFER_INFO
	r, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&csbi)))
	if r == 0 {
		return
	}

	curW := csbi.Window.Right - csbi.Window.Left + 1
	curH := csbi.Window.Bottom - csbi.Window.Top + 1

	if curW >= minCols && curH >= minRows {
		return
	}

	targetW := minCols
	if curW > targetW {
		targetW = curW
	}
	targetH := minRows
	if curH > targetH {
		targetH = curH
	}

	// Временно сжимаем окно до (1, 1), чтобы изменение буфера не вызывало конфликт
	minWindow := SMALL_RECT{0, 0, 1, 1}
	procSetConsoleWindowInfo.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&minWindow)))

	// Увеличиваем размер буфера
	targetBuf := COORD{
		X: targetW,
		Y: csbi.Size.Y,
	}
	if targetBuf.Y < targetH {
		targetBuf.Y = targetH
	}
	procSetConsoleScreenBufferSize.Call(uintptr(h), *(*uintptr)(unsafe.Pointer(&targetBuf)))

	// Устанавливаем целевой размер окна
	targetWindow := SMALL_RECT{
		Left:   0,
		Top:    0,
		Right:  targetW - 1,
		Bottom: targetH - 1,
	}
	procSetConsoleWindowInfo.Call(uintptr(h), 1, uintptr(unsafe.Pointer(&targetWindow)))
}
