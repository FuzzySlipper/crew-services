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
