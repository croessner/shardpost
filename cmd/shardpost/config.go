package main

import (
	"fmt"
	"os"
	"strings"

	"shardpost/internal/config"
)

func loadConfig(args []string) (*config.Config, string, error) {
	// The new name wins when both are supplied, including an explicitly empty value.
	path, ok := os.LookupEnv("SHARDPOST_CONFIG")
	if !ok {
		path = os.Getenv("IMAP_CONFIG")
	}

	command := commandServe

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagConfig:
			i++
			if i >= len(args) {
				return nil, "", fmt.Errorf("--config needs a path")
			}

			path = args[i]
		case commandScrub, commandPush, commandRunner, flagBatch, flagSweep, flagRepair, flagRestart:
			var err error
			if command, err = runCommand(command, args[i]); err != nil {
				return nil, "", err
			}
		case "index":
			n, cmd, err := indexCommand(args[i+1:])
			if err != nil {
				return nil, "", err
			}

			i += n
			command = cmd
		case "config":
			if i+1 >= len(args) || args[i+1] != commandCheck {
				return nil, "", fmt.Errorf("expected config check")
			}

			i++
			command = commandCheck
		default:
			return nil, "", fmt.Errorf("unknown argument")
		}
	}

	c, err := loadFor(command, path)

	return c, command, err
}

// loadFor loads the configuration for a command: the runner reads the
// servers' configuration without its authentication secrets
// (config.LoadRunner).
func loadFor(command, path string) (*config.Config, error) {
	if command == commandRunner {
		return config.LoadRunner(path)
	}

	return config.Load(path)
}

// Flags of scrub: --batch submits the batch job (commandScrubBatch),
// --sweep runs the sweep (commandScrubSweep), which --repair lets delete
// and --restart starts over.
const (
	flagConfig  = "--config"
	flagBatch   = "--batch"
	flagSweep   = "--sweep"
	flagRepair  = "--repair"
	flagRestart = "--restart"
)

// runCommand applies a command word: scrub, push, runner, or a flag of
// scrub.
func runCommand(command, arg string) (string, error) {
	switch arg {
	case flagBatch, flagSweep:
		if command != commandScrub {
			return "", fmt.Errorf("%s belongs to scrub", arg)
		}

		if arg == flagBatch {
			return commandScrubBatch, nil
		}

		return commandScrubSweep, nil
	case flagRepair, flagRestart:
		if !strings.HasPrefix(command, commandScrubSweep) || strings.Contains(command, arg) {
			return "", fmt.Errorf("%s belongs to scrub --sweep (once)", arg)
		}

		return sweepCommand(strings.Contains(command, flagRepair) || arg == flagRepair,
			strings.Contains(command, flagRestart) || arg == flagRestart), nil
	}

	return arg, nil
}

// sweepCommand is the sweep command with its flags in a fixed order.
func sweepCommand(repair, restart bool) string {
	command := commandScrubSweep
	if repair {
		command += " " + flagRepair
	}

	if restart {
		command += " " + flagRestart
	}

	return command
}

// showCommand builds "index show kv|learning" (command) with the keys
// or job ids that follow it.
func showCommand(command string, keys []string) (int, string, error) {
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, " \t") || strings.HasPrefix(k, "-") {
			return 0, "", fmt.Errorf("invalid key %q", k)
		}
	}

	return 2 + len(keys), strings.Join(append([]string{command}, keys...), " "), nil
}

// indexCommand parses what follows "index": "backfill accounts|kv|
// learning" and an optional "--restart", or "show kv|learning" and the
// admin store keys or learning job ids to show (the rest of the line;
// they become part of the command, separated by spaces). It returns the
// number of arguments it used.
func indexCommand(args []string) (int, string, error) {
	const accounts, kv, learning = "accounts", "kv", "learning"

	backfills := map[string][2]string{
		accounts: {commandBackfillAccounts, commandBackfillRestart},
		kv:       {commandBackfillKV, commandBackfillKVRestart},
		learning: {commandBackfillLearning, commandBackfillLearningRestart},
	}
	shows := map[string]string{kv: commandShowKV, learning: commandShowLearning}

	if len(args) >= 2 && args[0] == "show" && shows[args[1]] != "" {
		return showCommand(shows[args[1]], args[2:])
	}

	if len(args) < 2 || args[0] != "backfill" || backfills[args[1]][0] == "" {
		return 0, "", fmt.Errorf("expected index backfill accounts|kv|learning [--restart] or index show kv|learning")
	}

	if len(args) > 2 && args[2] == "--restart" {
		return 3, backfills[args[1]][1], nil
	}

	return 2, backfills[args[1]][0], nil
}
