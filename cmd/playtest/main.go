// playtest is a thin command-line and MCP client for the local playtest service.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"crew-services/internal/playtest/client"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "playtest:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, input io.Reader, output io.Writer) error {
	url := strings.TrimSpace(getenv("PLAYTEST_URL"))
	if url == "" {
		url = client.DefaultURL
	}
	global := flag.NewFlagSet("playtest", flag.ContinueOnError)
	global.SetOutput(io.Discard)
	global.StringVar(&url, "url", url, "local playtest service URL")
	if err := global.Parse(args); err != nil {
		return err
	}
	arguments := global.Args()
	if len(arguments) == 0 {
		return usageError()
	}
	service, err := client.New(url, nil)
	if err != nil {
		return err
	}
	if arguments[0] == "mcp" {
		if len(arguments) != 1 {
			return errors.New("mcp accepts no positional arguments")
		}
		return client.ServeMCP(ctx, input, output, service)
	}
	command, err := cliCommand(arguments)
	if err != nil {
		return err
	}
	result, err := service.Result(ctx, command)
	if err != nil {
		return err
	}
	return writeJSON(output, result)
}

func cliCommand(args []string) (client.Request, error) {
	if len(args) == 0 {
		return client.Request{}, usageError()
	}
	switch args[0] {
	case "games":
		if len(args) != 1 {
			return client.Request{}, errors.New("usage: playtest games")
		}
		return client.Request{Op: "games"}, nil
	case "game":
		if len(args) != 3 || args[1] != "show" {
			return client.Request{}, errors.New("usage: playtest game show GAME")
		}
		return client.Request{Op: "game", Game: args[2]}, nil
	case "start":
		if len(args) != 2 {
			return client.Request{}, errors.New("usage: playtest start GAME")
		}
		return client.Request{Op: "start", Game: args[1]}, nil
	case "status":
		if len(args) > 2 {
			return client.Request{}, errors.New("usage: playtest status [SESSION]")
		}
		request := client.Request{Op: "status"}
		if len(args) == 2 {
			request.SessionID = args[1]
		}
		return request, nil
	case "observe", "cancel", "recover", "stop":
		if len(args) != 2 {
			return client.Request{}, fmt.Errorf("usage: playtest %s SESSION", args[0])
		}
		return client.Request{Op: args[0], SessionID: args[1]}, nil
	case "input":
		if len(args) < 2 {
			return client.Request{}, errors.New("usage: playtest input SESSION --json JSON")
		}
		jsonText, err := requiredJSONFlag("input", args[2:])
		if err != nil {
			return client.Request{}, err
		}
		steps, err := client.NormalizeSteps(json.RawMessage(jsonText))
		if err != nil {
			return client.Request{}, fmt.Errorf("input --json: %w", err)
		}
		return client.Request{Op: "input", SessionID: args[1], Steps: steps}, nil
	case "browser", "capture", "interaction":
		if len(args) < 2 {
			if args[0] == "interaction" {
				return client.Request{}, errors.New("usage: playtest interaction SESSION [--json JSON]")
			}
			return client.Request{}, fmt.Errorf("usage: playtest %s SESSION --json JSON", args[0])
		}
		data, err := optionalJSONFlag(args[0], args[2:])
		if err != nil {
			return client.Request{}, err
		}
		if args[0] == "browser" && len(data) == 0 {
			return client.Request{}, errors.New("browser requires --json operation")
		}
		if args[0] == "interaction" && len(data) == 0 {
			data = json.RawMessage("{}")
		}
		return client.Request{Op: args[0], SessionID: args[1], Data: data}, nil
	case "run":
		if len(args) < 2 {
			return client.Request{}, errors.New("usage: playtest run SESSION --file PATH [--budget-ms N]")
		}
		flags := flag.NewFlagSet("run", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		var path string
		var budget int
		flags.StringVar(&path, "file", "", "script file")
		flags.IntVar(&budget, "budget-ms", 0, "execution budget")
		if err := flags.Parse(args[2:]); err != nil {
			return client.Request{}, err
		}
		if len(flags.Args()) != 0 || strings.TrimSpace(path) == "" {
			return client.Request{}, errors.New("usage: playtest run SESSION --file PATH [--budget-ms N]")
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return client.Request{}, fmt.Errorf("read script %s: %w", path, err)
		}
		request := client.Request{Op: "run", SessionID: args[1], Source: string(source)}
		if budget < 0 || (budget > 0 && (budget < 100 || budget > 120000)) {
			return client.Request{}, errors.New("--budget-ms must be 100..120000")
		}
		if budget > 0 {
			request.BudgetMS = &budget
		}
		return request, nil
	case "script":
		if len(args) != 2 {
			return client.Request{}, errors.New("usage: playtest script SCRIPTID")
		}
		return client.Request{Op: "script", ScriptID: args[1]}, nil
	case "resume":
		if len(args) < 2 {
			return client.Request{}, errors.New("usage: playtest resume SESSION [--json VALUE]")
		}
		data, err := optionalJSONFlag("resume", args[2:])
		if err != nil {
			return client.Request{}, err
		}
		return client.Request{Op: "resume", SessionID: args[1], Data: data}, nil
	default:
		return client.Request{}, usageError()
	}
}

func requiredJSONFlag(command string, args []string) (string, error) {
	value, err := optionalJSONFlag(command, args)
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "", fmt.Errorf("usage: playtest %s SESSION --json JSON", command)
	}
	return string(value), nil
}

func optionalJSONFlag(command string, args []string) (json.RawMessage, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var value string
	flags.StringVar(&value, "json", "", "JSON value")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if len(flags.Args()) != 0 {
		return nil, fmt.Errorf("usage: playtest %s SESSION [--json VALUE]", command)
	}
	if value == "" {
		return nil, nil
	}
	if !json.Valid([]byte(value)) {
		return nil, errors.New("--json must be valid JSON")
	}
	return json.RawMessage(value), nil
}

func writeJSON(output io.Writer, value json.RawMessage) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, value, "", "  "); err != nil {
		return fmt.Errorf("format playtest result: %w", err)
	}
	_, err := fmt.Fprintln(output, pretty.String())
	return err
}

func usageError() error {
	return errors.New("usage: playtest [--url URL] {games | game show GAME | start GAME | status [SESSION] | observe SESSION | interaction SESSION [--json JSON] | input SESSION --json JSON | browser SESSION --json JSON | capture SESSION [--json JSON] | run SESSION --file PATH [--budget-ms N] | script SCRIPTID | cancel SESSION | resume SESSION [--json VALUE] | recover SESSION | stop SESSION | mcp}")
}
