//go:build windows

// term_windows.go 平台相关：在 Windows 控制台上启用 ANSI/VT 转义处理。
//
// 为什么需要：Windows 控制台默认不解释 ANSI 转义序列，`\033[2K` 这类会被
// 静默丢弃 —— 表现为 watch 模式的逐帧输出堆叠刷屏（每帧都新起一段），
// 而不是原地覆盖。必须显式打开 ENABLE_VIRTUAL_TERMINAL_PROCESSING。
//
// 只依赖标准库：ENABLE_* 两个常量与 SetConsoleMode 都不在 syscall 包里
// （前者需本地定义，后者经 kernel32 动态获取），因此不必引入 golang.org/x/sys。
//
// 非 Windows 由 term_other.go 提供恒 true 实现（Unix 终端原生支持 VT）。
package main

import (
	"syscall"
)

const (
	// 控制台输出模式位。标准库 syscall 未定义这两个常量，按 Win32 文档取值。
	enableProcessedOutput           = 0x0001
	enableVirtualTerminalProcessing = 0x0004

	// SetConsoleMode 的 proc 名（kernel32）。
	procNameSetConsoleMode = "SetConsoleMode"
)

// enableVT 尝试在当前标准输出上启用 VT 处理。
//
// 返回 false 表示"无法启用"——最常见的原因是 stdout 被重定向到文件或管道
// （此时 GetConsoleMode 直接失败，没有控制台可设置）。调用方据此回落为
// 不带转义的逐帧滚动输出，保证 `stats.exe | tee log` 之类的用法仍可读。
func enableVT() bool {
	var mode uint32
	if err := syscall.GetConsoleMode(syscall.Stdout, &mode); err != nil {
		return false // 无控制台（重定向/管道）或调用失败
	}
	// 目标位已开则无需再设置（幂等；也避免不必要的 SetConsoleMode 调用）。
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	setConsoleMode := kernel32.NewProc(procNameSetConsoleMode)

	// 注意：Call 返回的 error 恒非 nil（其语义是 GetLastError），
	// 因此必须判首个返回值 r1 是否为 0，而不能判 err != nil。
	r1, _, _ := setConsoleMode.Call(
		uintptr(syscall.Stdout),
		uintptr(mode|enableProcessedOutput|enableVirtualTerminalProcessing),
	)
	return r1 != 0
}
