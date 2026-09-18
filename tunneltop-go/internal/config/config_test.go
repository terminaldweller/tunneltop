package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseOriginalStyle(t *testing.T) {
	in := `
[color]
header_fg = 4
header_bg = 0

[tunnel.socks5ir]
address = "127.0.0.1"
port = 9997
command = "autossh -M 0 -N -D 9997 -o ServerAliveInterval=180 -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -l debian -p 22 100.100.100.101"
test_command = 'curl -s -o /dev/null -w "%{http_code}" -k -I -4 --socks5 socks5h://127.0.0.1:9997 https://icanhazip.com'
test_command_result = "200"
test_interval = 300
test_timeout = 10
auto_start = false
`
	cfg, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Colors["header_fg"]; got != 4 {
		t.Fatalf("header_fg = %d", got)
	}
	if len(cfg.Tunnels) != 1 {
		t.Fatalf("len(tunnels) = %d", len(cfg.Tunnels))
	}
	tun := cfg.Tunnels[0]
	if tun.Name != "socks5ir" || tun.Port != 9997 || tun.AutoStart {
		t.Fatalf("bad tunnel: %#v", tun)
	}
	if tun.Command[0] != "autossh" {
		t.Fatalf("bad command: %#v", tun.Command)
	}
	if !reflect.DeepEqual(tun.TestCommand[len(tun.TestCommand)-1:], []string{"https://icanhazip.com"}) {
		t.Fatalf("bad test command: %#v", tun.TestCommand)
	}
}

func TestParseArrayCommand(t *testing.T) {
	in := `
[tunnel.web]
address = "127.0.0.1"
port = 8080
command = [
  "ssh",
  "-N",
  "-L", "8080:127.0.0.1:80",
  "user@example.com",
]
test_command = ["printf", "200"]
test_command_result = "200"
test_interval = 5
test_timeout = 1
auto_start = true
`
	cfg, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-N", "-L", "8080:127.0.0.1:80", "user@example.com"}
	if !reflect.DeepEqual(cfg.Tunnels[0].Command, want) {
		t.Fatalf("command = %#v, want %#v", cfg.Tunnels[0].Command, want)
	}
}
