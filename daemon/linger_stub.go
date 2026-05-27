//go:build !linux && !darwin

package daemon

func CheckLinger() (enabled bool, user string) {
	return true, ""
}
