// Command nova is the terminal coding agent.
//
//	 nova                          interactive, local agent
//	 nova -C ~/src/proj            start in a directory
//	 nova -p "fix the bug"         one-shot, prints the reply
//	 nova --cloud                   interactive, cloud session
//	 nova serve                      run the cloud backend
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/cloud"
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "nova: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		return runServe(os.Args[2:])
	}
	return runClient(os.Args[1:])
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "listen address")
	dir := fs.String("dir", "", "server data dir (default ~/.goagent/cloud)")
	token := fs.String("token", "", "bearer token for clients (empty = none)")
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	root := *dir
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		root = filepath.Join(home, ".goagent", "cloud")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	srv := cloud.NewServer(root, cfg, *token)
	httpSrv := &http.Server{
		Addr:         *addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams stay open
		IdleTimeout:  120 * time.Second,
	}
	fmt.Printf("nova serve: listening on %s (data: %s)\n", *addr, root)
	return httpSrv.ListenAndServe()
}

func runClient(args []string) error {
	fs := flag.NewFlagSet("nova", flag.ContinueOnError)
	workdir := fs.String("C", "", "working directory")
	prompt := fs.String("p", "", "one-shot prompt (print reply and exit)")
	model := fs.String("m", "", "model override (provider/model)")
	plan := fs.Bool("plan", false, "read-only planning mode")
	resume := fs.Bool("c", false, "resume the last saved session")
	themeName := fs.String("theme", "", "colour theme")
	noColor := fs.Bool("no-color", false, "disable colour")
	listModels := fs.Bool("models", false, "list configured models and exit")
	cloudMode := fs.Bool("cloud", false, "connect to a cloud server")
	server := fs.String("server", "http://localhost:8080", "cloud server address")
	sessionID := fs.String("session", "", "cloud session id (empty = create one)")
	token := fs.String("token", "", "cloud server bearer token")
	cfgPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *model != "" {
		cfg.DefaultModel = *model
	}
	if *themeName != "" {
		cfg.Theme = *themeName
	}
	if *noColor {
		theme.SetTrueColor(false)
	}

	if *listModels {
		return listModelsCmd(cfg)
	}

	app := tui.NewApp(cfg)
	if *plan {
		app.Mode = tui.ModePlan
	}
	if *resume {
		app.ResumeSession = "last"
	}
	if *workdir != "" {
		app.Workspace = *workdir
	} else {
		if cwd, err := os.Getwd(); err == nil {
			app.Workspace = cwd
		}
	}
	if app.SelectedModel == "" {
		app.SelectedModel = cfg.DefaultModel
	}

	// Positional words become the prompt too.
	rest := strings.Join(fs.Args(), " ")
	if *prompt == "" && rest != "" {
		prompt = &rest
	}

	if *cloudMode {
		return runCloud(app, *server, *sessionID, *token, *prompt)
	}

	if *prompt != "" {
		if err := app.InitLocalAgent(); err != nil {
			return err
		}
		out, err := app.RunOnce(*prompt)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	return app.Run("")
}

// listModelsCmd prints the configured providers and models.
func listModelsCmd(cfg *config.Config) error {
	for _, p := range cfg.Providers {
		if !p.Enabled {
			continue
		}
		fmt.Printf("%s:\n", p.Name)
		for _, m := range p.Models {
			mark := " "
			if cfg.DefaultModel == p.Name+"/"+m.ID || cfg.DefaultModel == m.ID {
				mark = "*"
			}
			fmt.Printf("  %s %s\n", mark, m.ID)
		}
	}
	return nil
}

// runCloud connects the TUI to a cloud session.
func runCloud(app *tui.App, serverAddr, sessionID, token, prompt string) error {
	client := cloud.NewClient(serverAddr, token)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if sessionID == "" {
		// Fresh session. Clone the local repo when inside one, so the cloud
		// session works on the same code.
		req := cloud.CreateRequest{Name: "terminal"}
		if repo, branch := gitRemote(app.Workspace); repo != "" {
			req.Repo = repo
			req.Branch = branch
		}
		info, err := client.Create(ctx, req)
		if err != nil {
			return fmt.Errorf("create cloud session: %w", err)
		}
		fmt.Printf("cloud session %s (%s)\n", info.ID, info.Name)
	} else {
		client.SetSession(sessionID)
		if _, err := client.List(ctx); err != nil {
			return fmt.Errorf("reach cloud server: %w", err)
		}
	}
	app.Cloud = client
	return app.Run(prompt)
}

// gitRemote returns the origin URL and current branch when dir is a git
// repo. Best effort: cloud mode works without a repo too.
func gitRemote(dir string) (repo, branch string) {
	if dir == "" {
		return "", ""
	}
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ""
	}
	repo = strings.TrimSpace(string(out))
	out, err = exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return repo, ""
	}
	branch = strings.TrimSpace(string(out))
	return repo, branch
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		return config.LoadFrom(path)
	}
	return config.Load()
}
