package crewcli

import (
	"bytes"
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestOnlyServeOrFlagsRunTheDriver(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		flags []string
		code  int
		ok    bool
		out   string
		err   string
	}{
		{name: "nothing", code: 2, err: "usage text"},
		{name: "a stray word", args: []string{"status"}, code: 2, err: `unknown command "status"`},
		{name: "help", args: []string{"help"}, out: "usage text"},
		{name: "--help", args: []string{"--help"}, out: "usage text"},
		{name: "serve", args: []string{"serve", "-listen", "x"}, flags: []string{"-listen", "x"}, ok: true},
		{name: "a unit's flags", args: []string{"-fabric-url", "x"}, flags: []string{"-fabric-url", "x"}, ok: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			flags, code, ok := ServeArgs(c.args, "usage text\n", &stdout, &stderr)
			if ok != c.ok || code != c.code || !reflect.DeepEqual(flags, c.flags) {
				t.Fatalf("ServeArgs = %v, %d, %v", flags, code, ok)
			}
			if !strings.Contains(stdout.String(), c.out) || !strings.Contains(stderr.String(), c.err) {
				t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestASecondDriverSaysTheFirstIsRunning(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, err = Listen(first.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("Listen = %v", err)
	}
}
