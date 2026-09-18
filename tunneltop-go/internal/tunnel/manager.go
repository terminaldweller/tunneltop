package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"tunneltop-go/internal/config"
)

type Status string

const (
	StatusUnknown  Status = "UNKWN"
	StatusUp       Status = "UP"
	StatusDown     Status = "DOWN"
	StatusTimeout  Status = "TMOUT"
	StatusDisabled Status = "OFF"
	StatusStarting Status = "START"
)

const maxLogLines = 1000

type EventKind int

const (
	EventStarted EventKind = iota
	EventExited
	EventStopped
	EventLog
	EventTestResult
	EventError
)

type Event struct {
	Kind   EventKind
	RunID  int64
	Name   string
	Stream string
	Line   string
	PID    int
	Status Status
	Stdout string
	Stderr string
	Err    error
	At     time.Time
}

type Snapshot struct {
	Name        string
	Address     string
	Port        int
	Status      Status
	Running     bool
	Disabled    bool
	PID         int
	Stdout      string
	Stderr      string
	StdoutLog   []string
	StderrLog   []string
	TestLog     []string
	LastError   string
	LastStarted time.Time
	LastTest    time.Time
	AutoStart   bool
}

type state struct {
	tunnel      config.Tunnel
	status      Status
	running     bool
	disabled    bool
	pid         int
	stdout      string
	stderr      string
	stdoutLog   []string
	stderrLog   []string
	testLog     []string
	lastError   string
	lastStarted time.Time
	lastTest    time.Time
	runID       int64
}

type runner struct {
	id      int64
	tunnel  config.Tunnel
	cancel  context.CancelFunc
	testNow chan struct{}
}

type Manager struct {
	ctx    context.Context
	events chan Event

	mu      sync.RWMutex
	nextID  int64
	order   []string
	states  map[string]*state
	runners map[string]*runner
}

func NewManager(ctx context.Context) *Manager {
	return &Manager{
		ctx:     ctx,
		events:  make(chan Event, 512),
		states:  make(map[string]*state),
		runners: make(map[string]*runner),
	}
}

func (m *Manager) Events() <-chan Event { return m.events }

func (m *Manager) Apply(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	desired := make(map[string]config.Tunnel, len(cfg.Tunnels))
	newOrder := make([]string, 0, len(cfg.Tunnels))
	for _, t := range cfg.Tunnels {
		desired[t.Name] = t
		newOrder = append(newOrder, t.Name)
	}

	for name := range m.states {
		if _, ok := desired[name]; !ok {
			m.stopLocked(name, false)
			delete(m.states, name)
		}
	}

	for _, t := range cfg.Tunnels {
		st, exists := m.states[t.Name]
		if exists && st.tunnel.EqualRuntime(t) {
			st.tunnel = t
			continue
		}

		keepStopped := false
		if exists {
			keepStopped = st.disabled
			m.stopLocked(t.Name, false)
		} else {
			st = &state{status: StatusUnknown, stdout: "n/a", stderr: "n/a"}
			m.states[t.Name] = st
		}

		st.tunnel = t
		st.pid = 0
		st.running = false
		st.lastError = ""
		st.stdout = "n/a"
		st.stderr = "n/a"
		st.stdoutLog = nil
		st.stderrLog = nil
		st.testLog = nil

		if t.AutoStart && !keepStopped {
			st.disabled = false
			m.startLocked(t.Name)
		} else {
			st.disabled = true
			st.status = StatusDisabled
		}
	}

	m.order = newOrder
}

func (m *Manager) Start(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked(name)
}

func (m *Manager) startLocked(name string) error {
	st, ok := m.states[name]
	if !ok {
		return fmt.Errorf("unknown tunnel %q", name)
	}
	if _, ok := m.runners[name]; ok {
		st.disabled = false
		return nil
	}

	m.nextID++
	id := m.nextID
	ctx, cancel := context.WithCancel(m.ctx)
	r := &runner{id: id, tunnel: st.tunnel, cancel: cancel, testNow: make(chan struct{}, 1)}
	m.runners[name] = r

	st.runID = id
	st.disabled = false
	st.status = StatusStarting
	st.running = false
	st.pid = 0
	st.lastError = ""

	go supervise(ctx, id, st.tunnel, m.events)
	go testScheduler(ctx, id, st.tunnel, r.testNow, m.events)
	return nil
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[name]; !ok {
		return fmt.Errorf("unknown tunnel %q", name)
	}
	m.stopLocked(name, true)
	return nil
}

func (m *Manager) stopLocked(name string, userRequested bool) {
	if r, ok := m.runners[name]; ok {
		r.cancel()
		delete(m.runners, name)
	}
	if st, ok := m.states[name]; ok {
		st.running = false
		st.pid = 0
		if userRequested {
			st.disabled = true
			st.status = StatusDisabled
		} else if st.status != StatusDisabled {
			st.status = StatusUnknown
		}
	}
}

func (m *Manager) Toggle(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[name]
	if !ok {
		return fmt.Errorf("unknown tunnel %q", name)
	}
	if _, running := m.runners[name]; running || !st.disabled {
		m.stopLocked(name, true)
		return nil
	}
	return m.startLocked(name)
}

func (m *Manager) Restart(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.states[name]; !ok {
		return fmt.Errorf("unknown tunnel %q", name)
	}
	m.stopLocked(name, false)
	return m.startLocked(name)
}

func (m *Manager) RunTestNow(name string) error {
	m.mu.RLock()
	r, ok := m.runners[name]
	_, exists := m.states[name]
	m.mu.RUnlock()
	if !exists {
		return fmt.Errorf("unknown tunnel %q", name)
	}
	if ok {
		select {
		case r.testNow <- struct{}{}:
		default:
		}
		return nil
	}

	// A stopped tunnel can still be tested once using its config.
	m.mu.RLock()
	st := m.states[name]
	id := st.runID
	t := st.tunnel
	m.mu.RUnlock()
	if len(t.TestCommand) == 0 {
		return fmt.Errorf("tunnel %q has no test_command", name)
	}
	go runTestOnce(m.ctx, id, t, m.events)
	return nil
}

func (m *Manager) ApplyEvent(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[ev.Name]
	if !ok {
		return
	}
	if ev.RunID != 0 && ev.RunID != st.runID {
		return
	}

	switch ev.Kind {
	case EventStarted:
		st.running = true
		st.pid = ev.PID
		st.status = StatusUnknown
		st.lastStarted = ev.At
	case EventExited:
		st.running = false
		st.pid = 0
		if ev.Err != nil {
			st.lastError = ev.Err.Error()
		}
	case EventStopped:
		st.running = false
		st.pid = 0
	case EventLog:
		if ev.Stream == "stdout" {
			st.stdout = ev.Line
			st.stdoutLog = appendBounded(st.stdoutLog, ev.Line)
		} else {
			st.stderr = ev.Line
			st.stderrLog = appendBounded(st.stderrLog, ev.Line)
		}
	case EventTestResult:
		st.status = ev.Status
		st.stdout = valueOrNA(ev.Stdout)
		st.stderr = valueOrNA(ev.Stderr)
		st.lastTest = ev.At
		st.testLog = appendBounded(st.testLog, formatTestLogEntry(ev))
		if ev.Err != nil {
			st.lastError = ev.Err.Error()
		}
	case EventError:
		if ev.Err != nil {
			st.lastError = ev.Err.Error()
		} else {
			st.lastError = ev.Line
		}
	}
}

func appendBounded(lines []string, line string) []string {
	lines = append(lines, line)
	if len(lines) <= maxLogLines {
		return lines
	}
	copy(lines, lines[len(lines)-maxLogLines:])
	return lines[:maxLogLines]
}

func formatTestLogEntry(ev Event) string {
	parts := []string{fmt.Sprintf("%s status=%s", ev.At.Format("2006-01-02 15:04:05"), ev.Status)}
	if strings.TrimSpace(ev.Stdout) != "" {
		parts = append(parts, "stdout="+ev.Stdout)
	}
	if strings.TrimSpace(ev.Stderr) != "" {
		parts = append(parts, "stderr="+ev.Stderr)
	}
	if ev.Err != nil {
		parts = append(parts, "err="+ev.Err.Error())
	}
	return strings.Join(parts, " | ")
}

func valueOrNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

func (m *Manager) Snapshots() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Snapshot, 0, len(m.order))
	for _, name := range m.order {
		st, ok := m.states[name]
		if !ok {
			continue
		}
		out = append(out, Snapshot{
			Name:        name,
			Address:     st.tunnel.Address,
			Port:        st.tunnel.Port,
			Status:      st.status,
			Running:     st.running,
			Disabled:    st.disabled,
			PID:         st.pid,
			Stdout:      st.stdout,
			Stderr:      st.stderr,
			StdoutLog:   append([]string(nil), st.stdoutLog...),
			StderrLog:   append([]string(nil), st.stderrLog...),
			TestLog:     append([]string(nil), st.testLog...),
			LastError:   st.lastError,
			LastStarted: st.lastStarted,
			LastTest:    st.lastTest,
			AutoStart:   st.tunnel.AutoStart,
		})
	}
	return out
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name := range m.runners {
		m.stopLocked(name, false)
	}
}

func supervise(ctx context.Context, id int64, t config.Tunnel, events chan<- Event) {
	defer sendEvent(ctx, events, Event{Kind: EventStopped, RunID: id, Name: t.Name})
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := runStreamingProcess(ctx, id, t.Name, t.Command, events)
		if ctx.Err() != nil {
			return
		}
		sendEvent(ctx, events, Event{Kind: EventExited, RunID: id, Name: t.Name, Err: err})
	}
}

func testScheduler(ctx context.Context, id int64, t config.Tunnel, testNow <-chan struct{}, events chan<- Event) {
	if len(t.TestCommand) == 0 {
		return
	}
	initial := time.NewTimer(5 * time.Second)
	defer initial.Stop()
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		var c <-chan time.Time
		if timer == nil {
			c = initial.C
		} else {
			c = timer.C
		}

		select {
		case <-ctx.Done():
			return
		case <-testNow:
			runTestOnce(ctx, id, t, events)
		case <-c:
			runTestOnce(ctx, id, t, events)
			if timer == nil {
				timer = time.NewTimer(time.Duration(t.TestInterval) * time.Second)
			} else {
				timer.Reset(time.Duration(t.TestInterval) * time.Second)
			}
		}
	}
}

func runTestOnce(parent context.Context, id int64, t config.Tunnel, events chan<- Event) {
	if len(t.TestCommand) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(t.TestTimeout)*time.Second)
	defer cancel()
	stdout, stderr, err := runCommandCollect(ctx, t.TestCommand)
	stdout = strings.Trim(strings.TrimSpace(stdout), "\"")
	stderr = strings.Trim(strings.TrimSpace(stderr), "\"")

	status := StatusDown
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = StatusTimeout
		} else {
			status = StatusDown
		}
	} else if stdout == t.TestCommandResult {
		status = StatusUp
	}

	sendEvent(parent, events, Event{
		Kind:   EventTestResult,
		RunID:  id,
		Name:   t.Name,
		Status: status,
		Stdout: stdout,
		Stderr: stderr,
		Err:    err,
		At:     time.Now(),
	})
}

func runStreamingProcess(ctx context.Context, id int64, name string, argv []string, events chan<- Event) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		sendEvent(ctx, events, Event{Kind: EventError, RunID: id, Name: name, Err: err})
		return err
	}
	sendEvent(ctx, events, Event{Kind: EventStarted, RunID: id, Name: name, PID: cmd.Process.Pid, At: time.Now()})

	var wg sync.WaitGroup
	wg.Add(2)
	go scanPipe(ctx, id, name, "stdout", stdout, events, &wg)
	go scanPipe(ctx, id, name, "stderr", stderr, events, &wg)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			killProcessGroup(cmd.Process.Pid)
			waitErr = <-done
		}
	}
	wg.Wait()
	return waitErr
}

func scanPipe(ctx context.Context, id int64, name, stream string, r io.Reader, events chan<- Event, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		sendEvent(ctx, events, Event{Kind: EventLog, RunID: id, Name: name, Stream: stream, Line: scanner.Text(), At: time.Now()})
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sendEvent(ctx, events, Event{Kind: EventError, RunID: id, Name: name, Err: fmt.Errorf("read %s: %w", stream, err)})
	}
}

func runCommandCollect(ctx context.Context, argv []string) (string, string, error) {
	if len(argv) == 0 {
		return "", "", fmt.Errorf("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return stdout.String(), stderr.String(), err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return stdout.String(), stderr.String(), err
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid)
		select {
		case err := <-done:
			return stdout.String(), stderr.String(), joinErr(ctx.Err(), err)
		case <-time.After(2 * time.Second):
			killProcessGroup(cmd.Process.Pid)
			err := <-done
			return stdout.String(), stderr.String(), joinErr(ctx.Err(), err)
		}
	}
}

func joinErr(primary, secondary error) error {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return fmt.Errorf("%w: %v", primary, secondary)
}

func terminateProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}

func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func sendEvent(ctx context.Context, ch chan<- Event, ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case ch <- ev:
	case <-ctx.Done():
	}
}
