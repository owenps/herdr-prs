package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/owenps/herdr-prs/internal/prs"
	"github.com/owenps/herdr-prs/internal/ui"
)

func openURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil {
		return errors.New("refusing to open a non-GitHub PR URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", raw, "--web")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot open GitHub: %s", string(out))
	}
	return nil
}

func execute(ctx context.Context, args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("herdr-prs", flag.ContinueOnError)
	open := flags.Bool("open", false, "open the installed Herdr popup")
	list := flags.Bool("list", false, "print a read-only live catalog as JSON")
	demo := flags.Bool("demo", false, "preview fixtures without GitHub or Herdr access")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments; use --help")
	}
	if *open {
		if *demo || *list {
			return errors.New("--open cannot be combined with --demo or --list")
		}
		return prs.NewHerdr().OpenPopup(ctx)
	}
	var actions ui.Actions
	if *demo {
		actions = ui.Actions{Load: demoCatalog, Detail: demoDetail,
			Jump:    func(context.Context, prs.Link) error { return errors.New("demo mode: navigation disabled") },
			OpenURL: func(context.Context, string) error { return errors.New("demo mode: browser disabled") }}
	} else {
		h := prs.NewHerdr()
		app := prs.NewApp(h)
		cached := prs.NewCachedApp(app)
		actions = ui.Actions{Load: app.Load, Detail: app.Detail, LoadStream: cached.Load,
			DetailStream: cached.Detail, Jump: h.Jump, OpenURL: openURL, AgentSnapshot: h.Snapshot}
	}
	if *list {
		catalog, err := actions.Load(ctx)
		if err != nil {
			return err
		}
		type row struct {
			prs.PullRequest
			Status string    `json:"status"`
			Group  prs.Group `json:"group"`
		}
		output := struct {
			PRs      []row    `json:"prs"`
			Warnings []string `json:"warnings"`
		}{PRs: make([]row, 0, len(catalog.PRs)), Warnings: catalog.Warnings}
		for _, p := range catalog.PRs {
			output.PRs = append(output.PRs, row{PullRequest: p, Status: p.Status(), Group: p.Group()})
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(output)
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("popup requires an interactive terminal; use --list or --demo --list")
	}
	return ui.Run(ctx, actions)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "herdr-prs:", err)
		os.Exit(1)
	}
}
