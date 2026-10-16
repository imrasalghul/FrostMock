package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"
)

// main is a login-only helper when built as an executable. Go plugin loading
// does not invoke it; there is no standalone inference server.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || args[0] != "login" {
		fmt.Fprintln(errout, "Usage: chatgpt-login login [browser] [--device|--headless] [--no-browser]")
		return 2
	}
	args = args[1:]
	if len(args) > 0 && args[0] == "browser" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(errout)
	device := flags.Bool("device", false, "Use device-code login")
	headless := flags.Bool("headless", false, "Use device-code login")
	noBrowser := flags.Bool("no-browser", false, "Print the browser URL without opening it")
	flags.Bool("verbose", false, "Show login progress")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(errout, "unknown login argument: %s\n", flags.Arg(0))
		return 2
	}
	l := &loginManager{}
	defer l.stop()
	if *device || *headless {
		state, err := l.start()
		if err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		fmt.Fprintf(out, "Open this URL and enter the code:\n%s\n\nCode: %s\n", state.VerifyURL, state.UserCode)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				fmt.Fprintln(errout, ctx.Err())
				return 1
			case <-ticker.C:
				state = l.snapshot()
				if state == nil {
					return 1
				}
				if state.Status == "error" {
					fmt.Fprintln(errout, state.Error)
					return 1
				}
				if state.Status == "success" {
					fmt.Fprintln(out, "Login successful. Tokens saved.")
					return 0
				}
			}
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		fmt.Fprintf(errout, "Cannot open OAuth callback on localhost:1455: %v; use login --device instead\n", err)
		return 1
	}
	err = l.browser(ctx, listener, func(authURL string) error {
		fmt.Fprintf(out, "Complete login in your browser:\n%s\n", authURL)
		if !*noBrowser {
			if err := openLoginBrowser(authURL); err != nil {
				fmt.Fprintln(errout, "Could not open browser automatically; open the URL above.")
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fmt.Fprintln(out, "Login successful. Tokens saved.")
	return 0
}

func openLoginBrowser(rawURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	case "darwin":
		command = exec.Command("open", rawURL)
	default:
		command = exec.Command("xdg-open", rawURL)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go command.Wait()
	return nil
}
