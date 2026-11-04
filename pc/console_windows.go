package main

import "golang.org/x/sys/windows"

// setupConsole — UTF-8 в консоли (для старого conhost).
func setupConsole() {
	windows.SetConsoleOutputCP(65001)
	windows.SetConsoleCP(65001)
}
