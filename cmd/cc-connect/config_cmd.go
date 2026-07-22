package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	ccconnect "github.com/yangzhousutpc-a11y/cc-connect-codex-sync"
	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/config"
)

func runConfig(args []string) {
	if len(args) == 0 {
		printConfigUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "example":
		fmt.Print(ccconnect.ConfigExampleTOML)
	case "init":
		runConfigInit(args[1:])
	case "format", "fmt":
		runConfigFormat(args[1:])
	case "path":
		fmt.Println(resolveConfigPath(""))
	default:
		fmt.Fprintf(os.Stderr, "Unknown config subcommand: %s\n", args[0])
		printConfigUsage()
		os.Exit(1)
	}
}

type configInitArgs struct {
	configPath string
	project    string
	workDir    string
}

func parseConfigInitArgs(args []string) (configInitArgs, error) {
	var result configInitArgs
	fs := flag.NewFlagSet("config init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&result.configPath, "config", "", "path to new config file")
	fs.StringVar(&result.project, "project", "", "project name")
	fs.StringVar(&result.workDir, "work-dir", "", "absolute project working directory")
	if err := fs.Parse(args); err != nil {
		return configInitArgs{}, err
	}
	if fs.NArg() != 0 {
		return configInitArgs{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if result.configPath == "" || result.project == "" || result.workDir == "" {
		return configInitArgs{}, fmt.Errorf("--config, --project, and --work-dir are required")
	}
	return result, nil
}

func runConfigInit(args []string) {
	parsed, err := parseConfigInitArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if err := config.InitCodexConfig(parsed.configPath, parsed.project, parsed.workDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created %s\n", parsed.configPath)
}

func runConfigFormat(args []string) {
	fs := flag.NewFlagSet("config format", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config file (default: auto-detect)")
	_ = fs.Parse(args)

	path := resolveConfigPath(*configPath)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Config file not found: %s\n", path)
		os.Exit(1)
	}

	if err := config.FormatConfigFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "Error formatting config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Formatted %s\n", path)
}

func printConfigUsage() {
	fmt.Fprintf(os.Stderr, `Usage: cc-connect config <subcommand>

Subcommands:
  example    Print a complete annotated config.toml example
  init       Create a minimal Codex config without overwriting an existing file
  format     Format the config file (alias: fmt)
  path       Print the resolved config file path

Flags for 'format':
  --config <path>   Path to config file (default: auto-detect)

Flags for 'init':
  --config <path>   New config file path (required)
  --project <name>  Project name (required)
  --work-dir <dir>  Existing absolute working directory (required)

Examples:
  cc-connect config example              Print example config
  cc-connect config example > config.toml  Save example config
  cc-connect config init --config /path/to/config.toml --project demo --work-dir /path/to/project
  cc-connect config format               Format default config file
  cc-connect config fmt --config /path/to/config.toml
`)
}
