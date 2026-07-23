package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/config"
	"golang.org/x/term"
)

type weComSecretReader func() (string, error)

func runWeCom(args []string) {
	err := runWeComCommand(args, os.Stdout, os.Stderr, func() (string, error) {
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		return string(value), err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runWeComCommand(args []string, stdout, stderr io.Writer, readSecret weComSecretReader) error {
	if len(args) == 0 {
		printWeComUsage(stdout)
		return nil
	}
	switch args[0] {
	case "help", "--help", "-h":
		printWeComUsage(stdout)
		return nil
	case "setup", "bind":
		return runWeComSetup(args[0], args[1:], stdout, stderr, readSecret)
	default:
		return fmt.Errorf("unknown wecom subcommand %q", args[0])
	}
}

func runWeComSetup(command string, args []string, stdout, stderr io.Writer, readSecret weComSecretReader) error {
	fs := flag.NewFlagSet("wecom "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project name")
	platformIndex := fs.Int("platform-index", 0, "1-based WeCom platform index (0 = first)")
	botID := fs.String("bot-id", "", "Enterprise WeChat intelligent-bot ID")
	botSecret := fs.String("bot-secret", "", "Enterprise WeChat intelligent-bot secret")
	allowFrom := fs.String("allow-from", "", "optional comma-separated allowed user IDs")
	if err := fs.Parse(args); err != nil {
		return err
	}

	projectName := strings.TrimSpace(*project)
	if projectName == "" {
		return fmt.Errorf("--project is required")
	}
	id := strings.TrimSpace(*botID)
	if id == "" {
		return fmt.Errorf("--bot-id is required")
	}
	secret := strings.TrimSpace(*botSecret)
	if secret == "" {
		if readSecret == nil {
			return fmt.Errorf("--bot-secret is required")
		}
		fmt.Fprint(stderr, "Bot Secret: ")
		value, err := readSecret()
		fmt.Fprintln(stderr)
		if err != nil {
			return fmt.Errorf("read bot secret: %w", err)
		}
		secret = strings.TrimSpace(value)
	}
	if secret == "" {
		return fmt.Errorf("bot secret is required")
	}

	initConfigPath(*configFile)
	workDir, _ := os.Getwd()
	provisioned, err := config.EnsureProjectWithWeComPlatform(config.EnsureProjectWithWeComOptions{
		ProjectName: projectName,
		WorkDir:     workDir,
	})
	if err != nil {
		return fmt.Errorf("prepare project: %w", err)
	}
	saved, err := config.SaveWeComPlatformCredentials(config.WeComCredentialUpdateOptions{
		ProjectName:   projectName,
		PlatformIndex: *platformIndex,
		BotID:         id,
		BotSecret:     secret,
		AllowFrom:     strings.TrimSpace(*allowFrom),
	})
	if err != nil {
		return fmt.Errorf("update config: %w", err)
	}

	if provisioned.Created {
		fmt.Fprintf(stdout, "已创建项目 %q。\n", projectName)
	} else if provisioned.AddedPlatform {
		fmt.Fprintf(stdout, "已为项目 %q 添加企业微信平台。\n", projectName)
	}
	fmt.Fprintf(stdout, "✅ 企业微信智能机器人已配置：项目 %q，Bot ID 尾号 %s。\n", saved.ProjectName, botIDSuffix(id))
	fmt.Fprintln(stdout, "下一步：重启服务，并在企业微信群中 @机器人 发送第一条消息。")
	return nil
}

func botIDSuffix(botID string) string {
	runes := []rune(strings.TrimSpace(botID))
	if len(runes) <= 4 {
		return string(runes)
	}
	return string(runes[len(runes)-4:])
}

func printWeComUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: cc-connect wecom <command> [options]

Commands:
  setup   Configure an Enterprise WeChat intelligent bot
  bind    Bind existing Enterprise WeChat bot credentials

Options:
  --config <path>          Path to config file
  --project <name>         Target project
  --platform-index <n>     1-based WeCom platform index (default: first)
  --bot-id <id>            Intelligent-bot ID
  --bot-secret <secret>    Intelligent-bot secret (prompted securely when omitted)
  --allow-from <ids>       Optional comma-separated allowed user IDs

Examples:
  cc-connect wecom setup --project my-project --bot-id BOT_ID
  cc-connect wecom bind --project my-project --bot-id BOT_ID --bot-secret BOT_SECRET`)
}
