// Copyright 2026.
// Use of this source code is governed by a BSD-style license.

// Command conmux mirrors a single interactive shell across every console
// device: it fans the shell's output out to all of them and merges input
// from all of them into the one shell. It is meant to be used as a u-root
// uinit (or initcmd) in a LinuxBoot initramfs, replacing the single-
// /dev/console shell that u-root's init would otherwise launch.
//
// Design
//
//   - The physical consoles (/dev/ttyS0, /dev/tty0, ...) are switched to raw
//     mode so no per-device echo or line discipline happens there.
//   - A pty pair is allocated. The shell runs on the slave (pts) with the pts
//     as its controlling terminal, so it keeps a normal cooked line discipline:
//     echo, line editing, ^C/job control all work.
//   - One goroutine copies pty-master -> every console (output fan-out).
//   - One goroutine per console copies that device -> pty-master (input fan-in).
//
// Because the pts is cooked, characters injected from any console are echoed
// by the pts line discipline back out the master, and the fan-out then mirrors
// that echo to *all* consoles -- so every operator sees a shared session.
//
// Build into a u-root image, e.g.:
//
//	u-root -build=bb -uinitcmd=conmux \
//	    core \
//	    github.com/u-root/u-root/cmds/core/gosh \
//	    ./cmds/exp/conmux
//
// and boot with multiple consoles, e.g. console=tty0 console=ttyS0,115200.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	shell    = flag.String("shell", "/bbin/gosh", "shell (or program) to run behind the mux")
	consoles = flag.String("consoles", "", "comma-separated console devices; default: parse console= from /proc/cmdline")
	baud     = flag.Uint("baud", 115200, "baud applied to serial devices that don't carry their own rate")
)

// baudRates maps an integer baud to its termios speed constant.
var baudRates = map[uint]uint32{
	9600: unix.B9600, 19200: unix.B19200, 38400: unix.B38400,
	57600: unix.B57600, 115200: unix.B115200, 230400: unix.B230400,
	460800: unix.B460800, 921600: unix.B921600,
}

type console struct {
	name string
	f    *os.File
	orig *unix.Termios // saved termios, for restore on exit
	baud uint
}

// consolesFromCmdline extracts console= devices (and any bauds) from
// /proc/cmdline, in order, de-duplicated, keeping the first baud per device.
func consolesFromCmdline() []console {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil
	}
	var out []console
	seen := map[string]bool{}
	for _, tok := range strings.Fields(string(b)) {
		v, ok := strings.CutPrefix(tok, "console=")
		if !ok || v == "" {
			continue
		}
		parts := strings.SplitN(v, ",", 2)
		dev := parts[0]
		// ttynull and netconsole are not usable interactive ttys.
		if dev == "" || dev == "ttynull" || strings.HasPrefix(dev, "netconsole") {
			continue
		}
		name := "/dev/" + dev
		if seen[name] {
			continue
		}
		seen[name] = true
		c := console{name: name}
		if len(parts) == 2 {
			c.baud = parseBaud(parts[1])
		}
		out = append(out, c)
	}
	return out
}

// parseBaud pulls the leading integer out of a console mode like "115200n8".
func parseBaud(mode string) uint {
	i := 0
	for i < len(mode) && mode[i] >= '0' && mode[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0
	}
	n, err := strconv.ParseUint(mode[:i], 10, 32)
	if err != nil {
		return 0
	}
	return uint(n)
}

// configure opens the device, saves its termios, switches it to raw mode, and
// applies a baud rate for serial ports. A device that is not a tty is still
// usable for write-only fan-out, so that case does not fail.
func (c *console) configure(defaultBaud uint) error {
	f, err := os.OpenFile(c.name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	c.f = f
	fd := int(f.Fd())

	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		// Not a tty: keep it open for output only.
		return nil
	}
	orig := *t
	c.orig = &orig

	// cfmakeraw equivalent.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	// Apply baud on serial-style devices only.
	if strings.HasPrefix(c.name, "/dev/ttyS") || strings.HasPrefix(c.name, "/dev/ttyUSB") {
		b := c.baud
		if b == 0 {
			b = defaultBaud
		}
		if sp, ok := baudRates[b]; ok {
			t.Cflag = (t.Cflag &^ unix.CBAUD) | sp
			t.Ispeed = sp
			t.Ospeed = sp
		}
	}

	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

func (c *console) restore() {
	if c.f == nil {
		return
	}
	if c.orig != nil {
		_ = unix.IoctlSetTermios(int(c.f.Fd()), unix.TCSETS, c.orig)
	}
	_ = c.f.Close()
}

// openPTY allocates a new pseudo-terminal pair, returning the master file and
// the slave's path. It copes with both the legacy /dev/ptmx node and a
// devpts-instance /dev/pts/ptmx.
func openPTY() (*os.File, string, error) {
	var (
		m   *os.File
		err error
	)
	for _, p := range []string{"/dev/ptmx", "/dev/pts/ptmx"} {
		m, err = os.OpenFile(p, os.O_RDWR|unix.O_NOCTTY, 0)
		if err == nil {
			break
		}
	}
	if m == nil {
		return nil, "", fmt.Errorf("opening ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = m.Close()
		return nil, "", fmt.Errorf("unlocking pts: %w", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = m.Close()
		return nil, "", fmt.Errorf("getting pts number: %w", err)
	}
	return m, fmt.Sprintf("/dev/pts/%d", n), nil
}

func selectConsoles() []console {
	if *consoles != "" {
		var cons []console
		for _, d := range strings.Split(*consoles, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			if !strings.HasPrefix(d, "/") {
				d = "/dev/" + d
			}
			cons = append(cons, console{name: d})
		}
		return cons
	}
	return consolesFromCmdline()
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("conmux: ")

	// Make sure devpts is mounted so slave allocation works. If u-root's init
	// already mounted it, this is a harmless EBUSY.
	_ = os.MkdirAll("/dev/pts", 0o755)
	_ = unix.Mount("devpts", "/dev/pts", "devpts", 0, "mode=0620,ptmxmode=0666,gid=5")

	cons := selectConsoles()
	if len(cons) == 0 {
		// Degrade to ordinary single-console behavior.
		cons = []console{{name: "/dev/console"}}
	}

	// Announce the plan *before* switching anything to raw, so these lines
	// still print with normal CRLF handling.
	var active []*console
	for i := range cons {
		log.Printf("attaching %s", cons[i].name)
	}
	for i := range cons {
		if err := cons[i].configure(*baud); err != nil {
			log.Printf("skipping %s: %v", cons[i].name, err)
			continue
		}
		active = append(active, &cons[i])
	}
	if len(active) == 0 {
		log.Fatal("no usable console devices")
	}
	defer func() {
		for _, c := range active {
			c.restore()
		}
	}()

	ptm, slavePath, err := openPTY()
	if err != nil {
		log.Fatalf("allocating pty: %v", err)
	}
	pts, err := os.OpenFile(slavePath, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		log.Fatalf("opening pts %s: %v", slavePath, err)
	}

	// Serial has no SIGWINCH; pin a sane window size so TUIs behave.
	_ = unix.IoctlSetWinsize(int(ptm.Fd()), unix.TIOCSWINSZ,
		&unix.Winsize{Row: 25, Col: 80})

	cmd := exec.Command(*shell)
	cmd.Args = append([]string{*shell}, flag.Args()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.Env = append(os.Environ(), "TERM=vt100")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // index into the child's fds: stdin, which is the pts
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("starting %s: %v", *shell, err)
	}
	_ = pts.Close() // the child holds its own dup; drive everything via master

	// Output fan-out: shell -> every console.
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := ptm.Read(buf)
			if n > 0 {
				for _, c := range active {
					_, _ = c.f.Write(buf[:n])
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Input fan-in: every console -> shell.
	for _, c := range active {
		go func(c *console) { _, _ = io.Copy(ptm, c.f) }(c)
	}

	err = cmd.Wait()
	_ = ptm.Close()
	// \r\n because the consoles are in raw mode now (no ONLCR).
	if err != nil {
		fmt.Fprintf(os.Stderr, "conmux: %s exited: %v\r\n", *shell, err)
	} else {
		fmt.Fprintf(os.Stderr, "conmux: %s exited\r\n", *shell)
	}
}
