// Package capture serves each monitor as a live H.264 stream.
//
// The difference from the shell script this replaces is who owns the socket.
// ffmpeg used to listen itself, which meant it served exactly one connection
// and exited when the client went away, so the host needed a restart loop
// around it. Here the server owns the listener and starts an encoder per
// connection: reconnecting works by construction, and — the part that actually
// matters on a laptop — nothing is encoded while no one is watching.
package capture

import (
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/vr-meta/linux-vr/host/internal/config"
	"github.com/vr-meta/linux-vr/host/internal/monitors"
)

// Service holds one listener per monitor.
type Service struct {
	cfg    config.Capture
	bind   string
	layout monitors.Layout

	mu      sync.Mutex
	current map[int]*encoder // monitor index -> the encoder feeding it
}

type encoder struct {
	cmd       *exec.Cmd
	conn      net.Conn
	connector string
	since     time.Time
}

// Connection is one window watching one screen. Reported so the tray icon and
// the settings page can answer "is anything actually looking at this machine",
// which is otherwise a question you answer by reading the log.
type Connection struct {
	Monitor   int       `json:"monitor"`
	Connector string    `json:"connector"`
	Client    string    `json:"client"`
	Since     time.Time `json:"since"`
}

func New(cfg config.Capture, bind string, layout monitors.Layout) *Service {
	return &Service{cfg: cfg, bind: bind, layout: layout, current: map[int]*encoder{}}
}

// Available reports whether capture can work at all. The client is told, so a
// missing encoder shows up as "this server has no video" rather than as a
// window that never paints.
func (s *Service) Available() (bool, string) {
	if !config.HasCommand("wl-screenrec") {
		return false, "wl-screenrec is not installed"
	}
	if len(s.layout.Monitors) == 0 {
		return false, "no monitors detected"
	}
	return true, ""
}

// Serve starts a listener for every monitor and blocks until they all fail.
func (s *Service) Serve() error {
	if len(s.layout.Monitors) == 0 {
		return fmt.Errorf("no monitors to capture")
	}
	errs := make(chan error, len(s.layout.Monitors))
	for _, m := range s.layout.Monitors {
		go func(m monitors.Monitor) { errs <- s.serveMonitor(m) }(m)
	}
	return <-errs
}

func (s *Service) serveMonitor(m monitors.Monitor) error {
	address := net.JoinHostPort(s.bind, strconv.Itoa(m.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("monitor %d: %w", m.Index, err)
	}
	log.Printf("[capture] monitor %d (%s) %dx%d on :%d",
		m.Index, m.Connector, m.Width, m.Height, m.Port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.stream(m, conn)
	}
}

// stream encodes for exactly one client.
//
// A second client on the same monitor replaces the first rather than joining
// it: two kmsgrab captures of one CRTC cost twice the GPU for the same picture,
// and the case this actually happens in is a window that was closed without the
// socket noticing yet. The newest window is the one someone is looking at.
func (s *Service) stream(m monitors.Monitor, conn net.Conn) {
	defer conn.Close()

	cmd := s.command(m)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[capture] monitor %d: %v", m.Index, err)
		return
	}
	cmd.Stderr = prefixWriter{fmt.Sprintf("[capture %d] ", m.Index)}
	// ffmpeg reads its input from KMS, not from stdin; leaving stdin attached
	// to the server's terminal makes it swallow keystrokes meant for the log.
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		log.Printf("[capture] monitor %d: cannot start ffmpeg: %v", m.Index, err)
		return
	}
	log.Printf("[capture] monitor %d: client %s, encoding", m.Index, conn.RemoteAddr())

	s.replace(m.Index, &encoder{cmd: cmd, conn: conn, connector: m.Connector, since: time.Now()})

	// The client never sends anything on this socket, so a read returning is
	// how a closed window is noticed. Without it, an encoder for a window that
	// went away keeps running until the TCP stack times out.
	go func() {
		var scratch [1]byte
		_, _ = conn.Read(scratch[:])
		stop(cmd)
	}()

	_, copyErr := io.Copy(conn, stdout)
	stop(cmd)
	_ = cmd.Wait()
	s.clear(m.Index, cmd)

	if copyErr != nil {
		log.Printf("[capture] monitor %d: client gone (%v)", m.Index, copyErr)
	} else {
		log.Printf("[capture] monitor %d: encoder stopped", m.Index)
	}
}

func (s *Service) replace(index int, e *encoder) {
	s.mu.Lock()
	previous := s.current[index]
	s.current[index] = e
	s.mu.Unlock()

	if previous != nil {
		log.Printf("[capture] monitor %d: a newer client took over", index)
		_ = previous.conn.Close()
		stop(previous.cmd)
	}
}

func (s *Service) clear(index int, cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.current[index]; e != nil && e.cmd == cmd {
		delete(s.current, index)
	}
}

// SetConfig changes the encoder settings.
//
// It applies to the next encoder that starts, not to one already running:
// ffmpeg's flags are fixed at exec, and restarting a live stream to change a
// quantiser would blank the window someone is working in.
func (s *Service) SetConfig(cfg config.Capture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

func (s *Service) settings() config.Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Streaming reports which monitors are being encoded right now.
func (s *Service) Streaming() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := make(map[int]bool, len(s.current))
	for index := range s.current {
		active[index] = true
	}
	return active
}

// Connections is the same answer with the client attached.
func (s *Service) Connections() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := make([]Connection, 0, len(s.current))
	for index, e := range s.current {
		client := ""
		if e.conn != nil {
			// The port half is noise here: what matters is which headset.
			if host, _, err := net.SplitHostPort(e.conn.RemoteAddr().String()); err == nil {
				client = host
			} else {
				client = e.conn.RemoteAddr().String()
			}
		}
		found = append(found, Connection{
			Monitor: index, Connector: e.connector, Client: client, Since: e.since,
		})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Monitor < found[j].Monitor })
	return found
}

// command builds the encoder invocation.
//
// Every flag here was chosen against a measurement; the reasoning is kept
// because it is the part that is expensive to rediscover.
func (s *Service) command(m monitors.Monitor) *exec.Cmd {
	codec := s.cfg.Codec
	muxer := codec
	if codec == "avc" {
		muxer = "h264"
	}

	args := []string{
		"--output", m.Connector,
		"--max-fps", "30",
		"--codec", codec,
		// "--no-damage",
		"--bitrate", "1 MB",
		"--ffmpeg-muxer", muxer,
		"--filename", "pipe:1",
	}

	if codec == "hevc" {
		args = append(args, "--low-power", "off")
	}

	return exec.Command("wl-screenrec", args...)
}

// stop ends an encoder.
//
// Closing the pipe is usually enough — ffmpeg gets EPIPE on its next write and
// exits — but a still desktop under CQP can go a second between writes, and
// under sudo the signal has to travel one process further. sudo relays what it
// receives to the command, so SIGTERM to the wrapper reaches ffmpeg; SIGKILL
// after a grace period covers the case where it does not.
func stop(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(2 * time.Second)
		_ = cmd.Process.Signal(syscall.SIGKILL)
	}()
}

// prefixWriter tags ffmpeg's diagnostics with the monitor they belong to.
type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(p []byte) (int, error) {
	log.Printf("%s%s", w.prefix, trimNewline(string(p)))
	return len(p), nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
