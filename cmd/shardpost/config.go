package main

import (
	"fmt"
	"os"
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
