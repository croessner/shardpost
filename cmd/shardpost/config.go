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
		case "--config":
			i++
			if i >= len(args) {
				return nil, "", fmt.Errorf("--config needs a path")
			}

			path = args[i]
		case commandScrub, commandPush:
			command = args[i]
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

	c, err := config.Load(path)

	return c, command, err
}

// showCommand builds "index show kv" with the keys that follow it.
func showCommand(keys []string) (int, string, error) {
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, " \t") || strings.HasPrefix(k, "-") {
			return 0, "", fmt.Errorf("invalid admin store key %q", k)
		}
	}

	return 2 + len(keys), strings.Join(append([]string{commandShowKV}, keys...), " "), nil
}

// indexCommand parses what follows "index": "backfill accounts" or
// "backfill kv" and an optional "--restart", or "show kv" and the admin
// store keys to show (the rest of the line; they become part of the
// command, separated by spaces). It returns the number of arguments it
// used.
func indexCommand(args []string) (int, string, error) {
	const accounts, kv = "accounts", "kv"

	if len(args) >= 2 && args[0] == "show" && args[1] == kv {
		return showCommand(args[2:])
	}

	if len(args) < 2 || args[0] != "backfill" || (args[1] != accounts && args[1] != kv) {
		return 0, "", fmt.Errorf("expected index backfill accounts, index backfill kv or index show kv")
	}

	plain, restart := commandBackfillAccounts, commandBackfillRestart
	if args[1] == kv {
		plain, restart = commandBackfillKV, commandBackfillKVRestart
	}

	if len(args) > 2 && args[2] == "--restart" {
		return 3, restart, nil
	}

	return 2, plain, nil
}
