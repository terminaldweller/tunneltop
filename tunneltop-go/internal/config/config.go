package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"tunneltop-go/internal/shellwords"
)

type Config struct {
	Colors  map[string]int
	Tunnels []Tunnel
}

type Tunnel struct {
	Name              string
	Address           string
	Port              int
	Command           []string
	TestCommand       []string
	TestCommandResult string
	TestInterval      int
	TestTimeout       int
	AutoStart         bool
}

func (t Tunnel) EqualRuntime(other Tunnel) bool {
	return t.Name == other.Name &&
		t.Address == other.Address &&
		t.Port == other.Port &&
		equalStrings(t.Command, other.Command) &&
		equalStrings(t.TestCommand, other.TestCommand) &&
		t.TestCommandResult == other.TestCommandResult &&
		t.TestInterval == other.TestInterval &&
		t.TestTimeout == other.TestTimeout &&
		t.AutoStart == other.AutoStart
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tunneltop.toml"
	}
	return filepath.Join(home, ".tunneltop.toml")
}

func ExpandPath(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func Load(path string) (Config, error) {
	path = ExpandPath(path)
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return Parse(f)
}

func ParseFile(path string) (Config, error) { return Load(path) }

func Parse(r interface{ Read([]byte) (int, error) }) (Config, error) {
	logical, err := readLogicalLines(r)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{Colors: map[string]int{}}
	tunnels := map[string]*Tunnel{}
	order := []string{}

	var section string
	var current *Tunnel
	var arrayTunnelIndex int

	for _, ln := range logical {
		line := strings.TrimSpace(stripComment(ln.text))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
			if name != "tunnels" && name != "tunnel" {
				return Config{}, fmt.Errorf("line %d: unsupported array table %q", ln.no, name)
			}
			section = "array-tunnel"
			arrayTunnelIndex++
			placeholder := fmt.Sprintf("__array_tunnel_%d", arrayTunnelIndex)
			t := &Tunnel{Name: placeholder, TestInterval: 300, TestTimeout: 10}
			tunnels[placeholder] = t
			order = append(order, placeholder)
			current = t
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			section = name
			current = nil
			if strings.HasPrefix(name, "tunnel.") {
				tunnelName := strings.TrimPrefix(name, "tunnel.")
				if tunnelName == "" {
					return Config{}, fmt.Errorf("line %d: empty tunnel name", ln.no)
				}
				t, ok := tunnels[tunnelName]
				if !ok {
					t = &Tunnel{Name: tunnelName, TestInterval: 300, TestTimeout: 10}
					tunnels[tunnelName] = t
					order = append(order, tunnelName)
				}
				current = t
			}
			continue
		}

		key, valueText, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: expected key = value", ln.no)
		}
		key = strings.TrimSpace(key)
		valueText = strings.TrimSpace(valueText)
		if key == "" {
			return Config{}, fmt.Errorf("line %d: empty key", ln.no)
		}

		value, err := parseValue(valueText)
		if err != nil {
			return Config{}, fmt.Errorf("line %d: %w", ln.no, err)
		}

		switch {
		case section == "color":
			i, ok := asInt(value)
			if !ok {
				return Config{}, fmt.Errorf("line %d: color %q must be an integer", ln.no, key)
			}
			cfg.Colors[key] = i

		case strings.HasPrefix(section, "tunnel.") || section == "array-tunnel":
			if current == nil {
				return Config{}, fmt.Errorf("line %d: tunnel field outside tunnel section", ln.no)
			}
			oldName := current.Name
			if err := applyTunnelField(current, key, value); err != nil {
				return Config{}, fmt.Errorf("line %d: %w", ln.no, err)
			}
			if section == "array-tunnel" && key == "name" && strings.HasPrefix(oldName, "__array_tunnel_") {
				newName := current.Name
				delete(tunnels, oldName)
				if _, exists := tunnels[newName]; exists {
					return Config{}, fmt.Errorf("line %d: duplicate tunnel name %q", ln.no, newName)
				}
				tunnels[newName] = current
				for i := range order {
					if order[i] == oldName {
						order[i] = newName
						break
					}
				}
			}

		default:
			return Config{}, fmt.Errorf("line %d: key %q outside a supported section", ln.no, key)
		}
	}

	seen := map[string]bool{}
	for _, name := range order {
		if seen[name] {
			continue
		}
		seen[name] = true
		t := *tunnels[name]
		if strings.HasPrefix(t.Name, "__array_tunnel_") {
			return Config{}, fmt.Errorf("array-style tunnel is missing name")
		}
		if t.Name == "" {
			return Config{}, fmt.Errorf("tunnel with empty name")
		}
		if len(t.Command) == 0 {
			return Config{}, fmt.Errorf("tunnel %q is missing command", t.Name)
		}
		cfg.Tunnels = append(cfg.Tunnels, t)
	}

	return cfg, nil
}

type logicalLine struct {
	no   int
	text string
}

func readLogicalLines(r interface{ Read([]byte) (int, error) }) ([]logicalLine, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []logicalLine
	var b strings.Builder
	startLine := 0
	balance := 0

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if b.Len() == 0 {
			startLine = lineNo
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		balance += bracketDelta(line)
		if balance <= 0 {
			out = append(out, logicalLine{no: startLine, text: b.String()})
			b.Reset()
			balance = 0
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if b.Len() > 0 {
		return nil, fmt.Errorf("line %d: unterminated array", startLine)
	}
	return out, nil
}

func bracketDelta(s string) int {
	inSingle := false
	inDouble := false
	escaped := false
	delta := 0
	for _, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if inDouble && r == '\\' {
			escaped = true
			continue
		}
		if !inDouble && r == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && r == '"' {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		switch r {
		case '[':
			delta++
		case ']':
			delta--
		}
	}
	return delta
}

func stripComment(s string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if inDouble && r == '\\' {
			escaped = true
			continue
		}
		if !inDouble && r == '\'' {
			inSingle = !inSingle
			continue
		}
		if !inSingle && r == '"' {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble && r == '#' {
			return s[:i]
		}
	}
	return s
}

func parseValue(s string) (any, error) {
	if s == "" {
		return nil, fmt.Errorf("empty value")
	}
	if strings.HasPrefix(s, "[") {
		return parseStringArray(s)
	}
	if strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") {
		return parseString(s)
	}
	if s == "true" {
		return true, nil
	}
	if s == "false" {
		return false, nil
	}
	if i, err := strconv.Atoi(s); err == nil {
		return i, nil
	}
	return nil, fmt.Errorf("unsupported value %q", s)
}

func parseString(s string) (string, error) {
	if len(s) < 2 {
		return "", fmt.Errorf("bad string")
	}
	quote := s[0]
	if quote != '\'' && quote != '"' {
		return "", fmt.Errorf("bad string quote")
	}
	if s[len(s)-1] != quote {
		return "", fmt.Errorf("unterminated string")
	}
	inner := s[1 : len(s)-1]
	if quote == '\'' {
		return inner, nil
	}
	q, err := strconv.Unquote(s)
	if err != nil {
		return "", err
	}
	return q, nil
}

func parseStringArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("bad array")
	}
	s = strings.TrimSpace(s[1 : len(s)-1])
	if s == "" {
		return []string{}, nil
	}

	var out []string
	for len(s) > 0 {
		s = strings.TrimLeftFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' })
		if s == "" {
			break
		}
		if s[0] != '\'' && s[0] != '"' {
			return nil, fmt.Errorf("array values must be strings")
		}
		quote := s[0]
		end := -1
		escaped := false
		for i := 1; i < len(s); i++ {
			ch := s[i]
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unterminated string in array")
		}
		item, err := parseString(s[:end+1])
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		s = strings.TrimSpace(s[end+1:])
		if strings.HasPrefix(s, ",") {
			s = strings.TrimSpace(s[1:])
		} else if s != "" {
			return nil, fmt.Errorf("expected comma in array")
		}
	}
	return out, nil
}

func applyTunnelField(t *Tunnel, key string, value any) error {
	switch key {
	case "name":
		v, ok := value.(string)
		if !ok || v == "" {
			return fmt.Errorf("name must be a non-empty string")
		}
		t.Name = v
	case "address":
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("address must be a string")
		}
		t.Address = v
	case "port":
		v, ok := asInt(value)
		if !ok {
			return fmt.Errorf("port must be an integer")
		}
		t.Port = v
	case "command":
		argv, err := commandValue(value)
		if err != nil {
			return fmt.Errorf("command: %w", err)
		}
		t.Command = argv
	case "test_command":
		argv, err := commandValue(value)
		if err != nil {
			return fmt.Errorf("test_command: %w", err)
		}
		t.TestCommand = argv
	case "test_command_result":
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("test_command_result must be a string")
		}
		t.TestCommandResult = v
	case "test_interval":
		v, ok := asInt(value)
		if !ok || v < 1 {
			return fmt.Errorf("test_interval must be a positive integer")
		}
		t.TestInterval = v
	case "test_timeout":
		v, ok := asInt(value)
		if !ok || v < 1 {
			return fmt.Errorf("test_timeout must be a positive integer")
		}
		t.TestTimeout = v
	case "auto_start":
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("auto_start must be a bool")
		}
		t.AutoStart = v
	default:
		return fmt.Errorf("unknown tunnel field %q", key)
	}
	return nil
}

func commandValue(value any) ([]string, error) {
	switch v := value.(type) {
	case string:
		return shellwords.Split(v)
	case []string:
		if len(v) == 0 {
			return nil, fmt.Errorf("empty command array")
		}
		return append([]string(nil), v...), nil
	default:
		return nil, fmt.Errorf("must be a string or string array")
	}
}

func asInt(v any) (int, bool) {
	i, ok := v.(int)
	return i, ok
}

func SortedNames(cfg Config) []string {
	out := make([]string, 0, len(cfg.Tunnels))
	for _, t := range cfg.Tunnels {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}
