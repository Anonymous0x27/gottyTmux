package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/codegangsta/cli"

	"github.com/yudai/gotty/backend/localcommand"
	"github.com/yudai/gotty/pkg/homedir"
	"github.com/yudai/gotty/server"
	"github.com/yudai/gotty/tmux"
	"github.com/yudai/gotty/utils"
)

func main() {
	app := cli.NewApp()
	app.Name = "gotty"
	app.Version = Version + "+" + CommitID
	app.Usage = "Share your terminal as a web application"
	app.HideHelp = true
	cli.AppHelpTemplate = helpTemplate

	appOptions := &server.Options{}
	if err := utils.ApplyDefaultValues(appOptions); err != nil {
		exit(err, 1)
	}
	backendOptions := &localcommand.Options{}
	if err := utils.ApplyDefaultValues(backendOptions); err != nil {
		exit(err, 1)
	}

	cliFlags, flagMappings, err := utils.GenerateFlags(appOptions, backendOptions)
	if err != nil {
		exit(err, 3)
	}

	app.Flags = append(
		cliFlags,
		cli.StringFlag{
			Name:   "config",
			Value:  "~/.gotty",
			Usage:  "Config file path",
			EnvVar: "GOTTY_CONFIG",
		},
	)

	app.Action = func(c *cli.Context) {
		configFile := c.String("config")
		_, err := os.Stat(homedir.Expand(configFile))
		if configFile != "~/.gotty" || !os.IsNotExist(err) {
			if err := utils.ApplyConfigFile(configFile, appOptions, backendOptions); err != nil {
				exit(err, 2)
			}
		}

		utils.ApplyFlags(cliFlags, flagMappings, c, appOptions, backendOptions)

		if c.IsSet("credential") {
			appOptions.EnableBasicAuth = true
		}
		if c.IsSet("password") || appOptions.Password != "" {
			appOptions.EnablePasswordAuth = true
		}
		if c.IsSet("tls-ca-crt") {
			appOptions.EnableTLSClientAuth = true
		}

		if !appOptions.Tmux && len(c.Args()) == 0 {
			msg := "Error: No command given."
			cli.ShowAppHelp(c)
			exit(errors.New(msg), 1)
		}

		err = appOptions.Validate()
		if err != nil {
			exit(err, 6)
		}

		hostname, _ := os.Hostname()
		var factory server.Factory
		var tmuxMgr tmux.TmuxManager

		if appOptions.Tmux {
			tmuxMgr = tmux.NewManager()
			if err := tmuxMgr.CheckInstalled(); err != nil {
				exit(fmt.Errorf("Error: %w", err), 4)
			}
			if !c.IsSet("permit-write") {
				appOptions.PermitWrite = true
			}
			factory, err = localcommand.NewTmuxFactory(tmuxMgr, backendOptions)
			if err != nil {
				exit(err, 3)
			}
			appOptions.TitleVariables = map[string]interface{}{
				"command":  "tmux",
				"argv":     []string{},
				"hostname": hostname,
			}
		} else {
			args := c.Args()
			factory, err = localcommand.NewFactory(args[0], args[1:], backendOptions)
			if err != nil {
				exit(err, 3)
			}
			appOptions.TitleVariables = map[string]interface{}{
				"command":  args[0],
				"argv":     args[1:],
				"hostname": hostname,
			}
		}

		srv, err := server.New(factory, appOptions)
		if err != nil {
			exit(err, 3)
		}
		if tmuxMgr != nil {
			srv.SetTmuxManager(tmuxMgr)
		}

		ctx, cancel := context.WithCancel(context.Background())
		gCtx, gCancel := context.WithCancel(context.Background())

		if appOptions.Tmux {
			log.Printf("GoTTY is starting in tmux mode (Web manager: /manager, Session list: /list)")
		} else {
			log.Printf("GoTTY is starting with command: %s", strings.Join(c.Args(), " "))
		}

		errs := make(chan error, 1)
		go func() {
			errs <- srv.Run(ctx, server.WithGracefullContext(gCtx))
		}()
		err = waitSignals(errs, cancel, gCancel)

		if err != nil && err != context.Canceled {
			fmt.Printf("Error: %s\n", err)
			exit(err, 8)
		}
	}
	app.Run(os.Args)
}

func exit(err error, code int) {
	if err != nil {
		fmt.Println(err)
	}
	os.Exit(code)
}

func waitSignals(errs chan error, cancel context.CancelFunc, gracefullCancel context.CancelFunc) error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(
		sigChan,
		syscall.SIGINT,
		syscall.SIGTERM,
	)

	select {
	case err := <-errs:
		return err

	case s := <-sigChan:
		switch s {
		case syscall.SIGINT:
			gracefullCancel()
			fmt.Println("C-C to force close")
			select {
			case err := <-errs:
				return err
			case <-sigChan:
				fmt.Println("Force closing...")
				cancel()
				return <-errs
			}
		default:
			cancel()
			return <-errs
		}
	}
}
