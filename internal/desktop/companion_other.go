//go:build !windows

package desktop

import "errors"

func companionRunning(string, string) bool { return false }

func openNative(string, string, bool) error {
	return errors.New("此平台暂不提供原生桌面窗口")
}
func closeCompanion(string, string) error {
	return errors.New("此平台暂不提供原生桌面窗口")
}
