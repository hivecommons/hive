//go:build unix

package tui

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// stdinPollingSupported gates the poll-based pump: on unix Run may wait for
// pumpFile because its poll interval bounds how long it can outlive `done`.
const stdinPollingSupported = true

// pumpPollIntervalMs is how often the stdin pump wakes to check whether the
// attach has ended. Short enough that Run's post-close wait is imperceptible,
// long enough that an idle attached terminal costs nothing measurable.
const pumpPollIntervalMs = 100

// pumpFile is the file-descriptor stdin pump: poll(2) with a timeout instead
// of a blocking read, so the goroutine can notice `done` and EXIT without
// consuming a byte. That is the whole point — a goroutine parked in a
// blocking read on the real terminal would survive the attach and race the
// resumed Bubble Tea program for the operator's next keystroke, swallowing
// it.
func pumpFile(done <-chan struct{}, f *os.File, send func([]byte) error) {
	fd := int32(f.Fd()) // #nosec G115 -- file descriptors are small non-negative ints that fit in int32 (poll(2) pollfd.fd)
	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
		n, err := unix.Poll(fds, pumpPollIntervalMs)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			// A descriptor poll(2) rejects outright. Give up on forwarding
			// input rather than degrade to a blocking read Run would then
			// deadlock waiting for; the attach stays readable and the remote
			// side can still end it.
			return
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLNVAL) != 0 {
			return
		}
		nr, rerr := f.Read(buf)
		if nr > 0 {
			if send(buf[:nr]) != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// watchResize forwards local terminal size changes to the remote session.
// SIGWINCH is the terminal's own resize notification; each one is answered
// with a fresh measurement rather than a delta, because signals coalesce.
func watchResize(done <-chan struct{}, f *os.File, resize func(cols, rows int) error) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	for {
		select {
		case <-done:
			return
		case <-winch:
			if w, h, err := term.GetSize(f.Fd()); err == nil && w > 0 && h > 0 {
				if resize(w, h) != nil {
					return
				}
			}
		}
	}
}
