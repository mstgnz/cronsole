//go:build !linux

package hostinfo

// read reports nothing on platforms with no /proc and no cgroups.
//
// Supported is false rather than a set of zeros, and the screen hides the panel
// entirely: a row of zeros reads as "nothing is wrong", which is the opposite of
// "this was never measured". The service is deployed on Linux; this exists so
// the build and the tests work on a developer's machine.
func (r *Reader) read() Stats {
	return Stats{Supported: false, CPUPercent: -1}
}
