//go:build !windows

// term_other.go 非 Windows 平台：终端原生支持 ANSI/VT 转义，无需启用动作。
// 与 term_windows.go 由构建标签互斥。
package main

// enableVT 在非 Windows 平台恒返回 true：Unix 终端默认解释 ANSI 转义序列，
// 不存在需要显式打开的开关。返回 true 让调用方直接走原地刷新路径。
func enableVT() bool { return true }
