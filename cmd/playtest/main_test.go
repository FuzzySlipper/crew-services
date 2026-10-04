package main

import "testing"

func TestCLICommandRoutesInputObjectSteps(t *testing.T) {
	command, err := cliCommand([]string{"input", "session-1", "--json", `{"steps":[{"kind":"key","key":"SPACE"}]}`})
	if err != nil {
		t.Fatal(err)
	}
	if command.Op != "input" || command.SessionID != "session-1" || string(command.Steps) != `[{"kind":"key","key":"SPACE"}]` {
		t.Fatalf("command = %+v", command)
	}
}

func TestCLICommandRejectsMissingRequiredInputJSON(t *testing.T) {
	if _, err := cliCommand([]string{"input", "session-1"}); err == nil {
		t.Fatal("input without --json succeeded")
	}
}

func TestCLICommandRoutesInteractionWithDefaultOptions(t *testing.T) {
	command, err := cliCommand([]string{"interaction", "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Op != "interaction" || command.SessionID != "session-1" || string(command.Data) != "{}" {
		t.Fatalf("command = %+v", command)
	}
}

func TestCLIStartTakesKeep(t *testing.T) {
	plain, err := cliCommand([]string{"start", "doom"})
	if err != nil || plain.Keep || plain.Game != "doom" {
		t.Fatalf("start doom = %+v %v", plain, err)
	}
	kept, err := cliCommand([]string{"start", "doom", "--keep"})
	if err != nil || !kept.Keep || kept.Game != "doom" {
		t.Fatalf("start doom --keep = %+v %v", kept, err)
	}
	for _, args := range [][]string{{"start", "--keep"}, {"start", "doom", "--other"}, {"start", "doom", "--keep", "x"}} {
		if _, err := cliCommand(args); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}
