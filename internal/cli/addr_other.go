//go:build !linux

package cli

func unusableIPv6() map[string]bool { return map[string]bool{} }
