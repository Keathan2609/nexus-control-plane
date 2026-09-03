// Command nexus is the NEXUS command-line interface.
//
// It talks to the same REST API the console does and holds no privileged access of its
// own: no database connection, no direct queue access, no bypass. That constraint is
// what keeps the API honest — anything the CLI can do, the console can do, and every
// authorisation decision happens in one place.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `nexus — delivery control plane

Usage:
  nexus <command> [arguments]

Commands:
  login                        Authenticate and store a token
  whoami                       Show the signed-in identity and permissions
  status                       Control plane health and queue depth

  project list                 List projects
  project get <id>             Show a project, its environments and recent runs

  pipeline run <project-id>    Trigger a pipeline run
  pipeline list [project-id]   List runs
  pipeline get <run-id>        Show a run and its steps
  pipeline logs <job-id>       Print a job's output
  pipeline cancel <run-id>     Cancel a running pipeline
  pipeline watch <run-id>      Follow a run until it reaches a terminal state

  deployment list              List deployments
  deployment get <id>          Show a deployment and its health checks
  deployment rollback <env-id> Roll an environment back to its last healthy release

  environment list             List environments

Flags:
  --api <url>    Control plane URL (default $NEXUS_API_URL or http://localhost:8080)
  --json         Emit raw JSON instead of a table
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "nexus: %v\n", err)
		os.Exit(1)
	}
}

var errUsage = errors.New("usage")

type client struct {
	baseURL string
	token   string
	http    *http.Client
	json    bool
}

func run(args []string) error {
	var positional []string
	c := &client{
		baseURL: envOr("NEXUS_API_URL", "http://localhost:8080"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--api":
			if i+1 >= len(args) {
				return fmt.Errorf("--api requires a URL")
			}
			c.baseURL = args[i+1]
			i++
		case "--json":
			c.json = true
		case "-h", "--help", "help":
			return errUsage
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) == 0 {
		return errUsage
	}
	c.token = loadToken()

	switch positional[0] {
	case "login":
		return c.login()
	case "whoami":
		return c.whoami()
	case "status":
		return c.status()
	case "project":
		return c.project(positional[1:])
	case "pipeline":
		return c.pipeline(positional[1:])
	case "deployment":
		return c.deployment(positional[1:])
	case "environment":
		return c.environment(positional[1:])
	default:
		return fmt.Errorf("unknown command %q", positional[0])
	}
}

// --- transport ----------------------------------------------------------------

type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("request failed with status %d", e.Status)
	}
	return e.Message
}

func (c *client) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the control plane at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(payload, &envelope)
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("not authenticated — run `nexus login`")
		}
		return &apiError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}

	if c.json {
		var pretty bytes.Buffer
		if json.Indent(&pretty, payload, "", "  ") == nil {
			fmt.Println(pretty.String())
		} else {
			fmt.Println(string(payload))
		}
		return errPrinted
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

// errPrinted signals that --json already wrote the response, so the command should stop
// without formatting a table. It is filtered out before reaching the user.
var errPrinted = errors.New("printed")

func (c *client) call(method, path string, body any, out any) error {
	err := c.do(method, path, body, out)
	if errors.Is(err, errPrinted) {
		os.Exit(0)
	}
	return err
}

// --- token storage --------------------------------------------------------------

func tokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".nexus-token"
	}
	return filepath.Join(home, ".nexus", "token")
}

func loadToken() string {
	if t := os.Getenv("NEXUS_TOKEN"); t != "" {
		return t
	}
	data, err := os.ReadFile(tokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveToken(token string) error {
	path := tokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// 0600: the token is a bearer credential, and a world-readable one on a shared
	// machine is equivalent to no credential at all.
	return os.WriteFile(path, []byte(token), 0o600)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func out() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}
