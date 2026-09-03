package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"
)

// --- identity -------------------------------------------------------------------

func (c *client) login() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Email: ")
	email, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	fmt.Print("Password: ")
	password, err := reader.ReadString('\n')
	if err != nil {
		return err
	}

	var res struct {
		Token string `json:"token"`
		User  struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"user"`
	}
	if err := c.call("POST", "/api/v1/auth/login", map[string]string{
		"email":    strings.TrimSpace(email),
		"password": strings.TrimSpace(password),
	}, &res); err != nil {
		return err
	}

	if err := saveToken(res.Token); err != nil {
		return fmt.Errorf("could not store the token: %w", err)
	}
	fmt.Printf("Signed in as %s (%s)\n", res.User.Email, res.User.Role)
	return nil
}

func (c *client) whoami() error {
	var res struct {
		User struct {
			Email string `json:"email"`
			Name  string `json:"name"`
			Role  string `json:"role"`
		} `json:"user"`
		Permissions []string `json:"permissions"`
	}
	if err := c.call("GET", "/api/v1/auth/me", nil, &res); err != nil {
		return err
	}

	w := out()
	fmt.Fprintf(w, "NAME\t%s\n", res.User.Name)
	fmt.Fprintf(w, "EMAIL\t%s\n", res.User.Email)
	fmt.Fprintf(w, "ROLE\t%s\n", res.User.Role)
	fmt.Fprintf(w, "GRANTS\t%d permissions\n", len(res.Permissions))
	return w.Flush()
}

func (c *client) status() error {
	var overview struct {
		Metrics struct {
			Projects      int     `json:"projects"`
			Environments  int     `json:"environments"`
			RunsRunning   int     `json:"runs_running"`
			QueueDepth    int     `json:"queue_depth"`
			JobsRunning   int     `json:"jobs_running"`
			WorkersLive   int     `json:"workers_live"`
			SuccessRate   float64 `json:"success_rate"`
			OpenDrift     int     `json:"open_drift"`
			OutboxPending int     `json:"outbox_pending"`
			Degraded      int     `json:"degraded_deployments"`
		} `json:"metrics"`
	}
	if err := c.call("GET", "/api/v1/overview", nil, &overview); err != nil {
		return err
	}

	m := overview.Metrics
	w := out()
	fmt.Fprintf(w, "PROJECTS\t%d\n", m.Projects)
	fmt.Fprintf(w, "ENVIRONMENTS\t%d\n", m.Environments)
	fmt.Fprintf(w, "SUCCESS RATE\t%.1f%%\n", m.SuccessRate)
	fmt.Fprintf(w, "RUNS IN FLIGHT\t%d\n", m.RunsRunning)
	fmt.Fprintf(w, "QUEUE DEPTH\t%d\n", m.QueueDepth)
	fmt.Fprintf(w, "JOBS RUNNING\t%d\n", m.JobsRunning)
	fmt.Fprintf(w, "WORKERS LIVE\t%d\n", m.WorkersLive)
	fmt.Fprintf(w, "DEGRADED\t%d\n", m.Degraded)
	fmt.Fprintf(w, "OPEN DRIFT\t%d\n", m.OpenDrift)
	fmt.Fprintf(w, "OUTBOX PENDING\t%d\n", m.OutboxPending)
	return w.Flush()
}

// --- projects --------------------------------------------------------------------

type projectRow struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Slug          string  `json:"slug"`
	Runtime       string  `json:"runtime"`
	Repository    string  `json:"repository"`
	SuccessRate   float64 `json:"success_rate"`
	LastRunNumber *int64  `json:"last_run_number"`
	LastRunState  *string `json:"last_run_state"`
	ProdRevision  *string `json:"production_revision"`
}

func (c *client) project(args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "list":
		var page struct {
			Data []projectRow `json:"data"`
		}
		if err := c.call("GET", "/api/v1/projects?limit=100", nil, &page); err != nil {
			return err
		}
		w := out()
		fmt.Fprintln(w, "ID\tNAME\tRUNTIME\tSUCCESS\tLAST RUN\tPRODUCTION")
		for _, p := range page.Data {
			fmt.Fprintf(w, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n",
				p.ID, p.Name, strings.ToLower(p.Runtime), p.SuccessRate,
				runRef(p.LastRunNumber, p.LastRunState), deref(p.ProdRevision))
		}
		return w.Flush()

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("project get requires a project id")
		}
		var res struct {
			Project      projectRow `json:"project"`
			Environments []struct {
				Name     string  `json:"name"`
				Kind     string  `json:"kind"`
				Revision *string `json:"revision"`
				Health   *string `json:"health_status"`
			} `json:"environments"`
			RecentRuns []runRow `json:"recent_runs"`
		}
		if err := c.call("GET", "/api/v1/projects/"+args[1], nil, &res); err != nil {
			return err
		}

		w := out()
		fmt.Fprintf(w, "NAME\t%s\n", res.Project.Name)
		fmt.Fprintf(w, "REPOSITORY\t%s\n", res.Project.Repository)
		fmt.Fprintf(w, "RUNTIME\t%s\n", strings.ToLower(res.Project.Runtime))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "ENVIRONMENT\tKIND\tREVISION\tHEALTH")
		for _, e := range res.Environments {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, strings.ToLower(e.Kind),
				deref(e.Revision), deref(e.Health))
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "RUN\tCOMMIT\tBRANCH\tSTATE")
		for _, r := range res.RecentRuns {
			fmt.Fprintf(w, "#%d\t%s\t%s\t%s\n", r.Number, short(r.CommitSHA), r.Branch, r.State)
		}
		return w.Flush()
	}
	return errUsage
}

// --- pipelines ---------------------------------------------------------------------

type runRow struct {
	ID          string `json:"id"`
	ProjectName string `json:"project_name"`
	Number      int64  `json:"number"`
	State       string `json:"state"`
	Branch      string `json:"branch"`
	CommitSHA   string `json:"commit_sha"`
	JobsDone    int    `json:"jobs_done"`
	JobsTotal   int    `json:"jobs_total"`
	DurationMS  *int64 `json:"duration_ms"`
}

type jobRow struct {
	ID          string `json:"id"`
	StepKey     string `json:"step_key"`
	State       string `json:"state"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	DurationMS  *int64 `json:"duration_ms"`
	ErrorCode   *string
	ErrorMsg    *string `json:"error_message"`
}

func (c *client) pipeline(args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "run":
		if len(args) < 2 {
			return fmt.Errorf("pipeline run requires a project id")
		}
		var res struct {
			ID     string `json:"id"`
			Number int64  `json:"number"`
			State  string `json:"state"`
		}
		if err := c.call("POST", "/api/v1/projects/"+args[1]+"/runs",
			map[string]string{"message": "Triggered from the CLI"}, &res); err != nil {
			return err
		}
		fmt.Printf("Queued run #%d (%s)\nFollow it with: nexus pipeline watch %s\n",
			res.Number, res.State, res.ID)
		return nil

	case "list":
		path := "/api/v1/runs?limit=30"
		if len(args) > 1 {
			path += "&project_id=" + args[1]
		}
		var page struct {
			Data []runRow `json:"data"`
		}
		if err := c.call("GET", path, nil, &page); err != nil {
			return err
		}
		w := out()
		fmt.Fprintln(w, "ID\tPROJECT\tRUN\tCOMMIT\tBRANCH\tJOBS\tDURATION\tSTATE")
		for _, r := range page.Data {
			fmt.Fprintf(w, "%s\t%s\t#%d\t%s\t%s\t%d/%d\t%s\t%s\n",
				r.ID, r.ProjectName, r.Number, short(r.CommitSHA), r.Branch,
				r.JobsDone, r.JobsTotal, ms(r.DurationMS), r.State)
		}
		return w.Flush()

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("pipeline get requires a run id")
		}
		res, err := c.fetchRun(args[1])
		if err != nil {
			return err
		}
		return printRun(res)

	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("pipeline logs requires a job id")
		}
		var res struct {
			Data []struct {
				Seq     int    `json:"seq"`
				Stream  string `json:"stream"`
				Message string `json:"message"`
			} `json:"data"`
		}
		if err := c.call("GET", "/api/v1/jobs/"+args[1]+"/logs?limit=1000", nil, &res); err != nil {
			return err
		}
		for _, line := range res.Data {
			if line.Stream == "stderr" {
				fmt.Fprintln(os.Stderr, line.Message)
			} else {
				fmt.Println(line.Message)
			}
		}
		return nil

	case "cancel":
		if len(args) < 2 {
			return fmt.Errorf("pipeline cancel requires a run id")
		}
		if err := c.call("POST", "/api/v1/runs/"+args[1]+"/cancel", nil, nil); err != nil {
			return err
		}
		fmt.Println("Run cancelled.")
		return nil

	case "watch":
		if len(args) < 2 {
			return fmt.Errorf("pipeline watch requires a run id")
		}
		return c.watch(args[1])
	}
	return errUsage
}

type runResponse struct {
	Run  runRow   `json:"run"`
	Jobs []jobRow `json:"jobs"`
}

func (c *client) fetchRun(id string) (*runResponse, error) {
	var res runResponse
	if err := c.call("GET", "/api/v1/runs/"+id, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func printRun(res *runResponse) error {
	w := out()
	fmt.Fprintf(w, "RUN\t#%d\n", res.Run.Number)
	fmt.Fprintf(w, "PROJECT\t%s\n", res.Run.ProjectName)
	fmt.Fprintf(w, "COMMIT\t%s@%s\n", res.Run.Branch, short(res.Run.CommitSHA))
	fmt.Fprintf(w, "STATE\t%s\n", res.Run.State)
	fmt.Fprintf(w, "DURATION\t%s\n", ms(res.Run.DurationMS))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "STEP\tSTATE\tATTEMPT\tDURATION\tJOB ID")
	for _, j := range res.Jobs {
		fmt.Fprintf(w, "%s\t%s\t%d/%d\t%s\t%s\n",
			j.StepKey, j.State, j.Attempt, j.MaxAttempts, ms(j.DurationMS), j.ID)
	}
	return w.Flush()
}

// watch polls a run until it reaches a terminal state.
//
// Polling rather than subscribing to the event stream: a CLI that follows one run for a
// few minutes does not justify a second transport and a second auth path, and the run is
// the source of truth either way.
func (c *client) watch(id string) error {
	terminal := map[string]bool{
		"SUCCEEDED": true, "FAILED": true, "CANCELLED": true, "TIMED_OUT": true,
	}
	seen := map[string]string{}

	for {
		res, err := c.fetchRun(id)
		if err != nil {
			return err
		}
		for _, j := range res.Jobs {
			if seen[j.StepKey] == j.State {
				continue
			}
			seen[j.StepKey] = j.State
			mark := stateMark(j.State)
			line := fmt.Sprintf("%s %-16s %s", mark, j.StepKey, strings.ToLower(j.State))
			if j.Attempt > 1 {
				line += fmt.Sprintf(" (attempt %d)", j.Attempt)
			}
			if j.DurationMS != nil && terminal[j.State] {
				line += "  " + ms(j.DurationMS)
			}
			fmt.Println(line)
			if j.ErrorMsg != nil && *j.ErrorMsg != "" {
				fmt.Fprintf(os.Stderr, "    %s\n", *j.ErrorMsg)
			}
		}

		if terminal[res.Run.State] {
			fmt.Printf("\nRun #%d %s in %s\n",
				res.Run.Number, strings.ToLower(res.Run.State), ms(res.Run.DurationMS))
			if res.Run.State != "SUCCEEDED" {
				os.Exit(1)
			}
			return nil
		}
		time.Sleep(time.Second)
	}
}

// --- deployments -------------------------------------------------------------------

func (c *client) deployment(args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "list":
		var page struct {
			Data []struct {
				ID          string `json:"id"`
				Environment string `json:"environment_name"`
				Number      int64  `json:"number"`
				Version     string `json:"release_version"`
				Revision    string `json:"desired_revision"`
				Sync        string `json:"sync_status"`
				Health      string `json:"health_status"`
				State       string `json:"state"`
				IsRollback  bool   `json:"is_rollback"`
			} `json:"data"`
		}
		if err := c.call("GET", "/api/v1/deployments?limit=30", nil, &page); err != nil {
			return err
		}
		w := out()
		fmt.Fprintln(w, "ID\tENVIRONMENT\tRELEASE\tREVISION\tSYNC\tHEALTH\tSTATE")
		for _, d := range page.Data {
			name := d.Environment
			if d.IsRollback {
				name += " (rollback)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				d.ID, name, d.Version, d.Revision, d.Sync, d.Health, d.State)
		}
		return w.Flush()

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("deployment get requires a deployment id")
		}
		var res struct {
			Deployment struct {
				Number   int64   `json:"number"`
				Env      string  `json:"environment_name"`
				Version  string  `json:"release_version"`
				Desired  string  `json:"desired_revision"`
				Actual   *string `json:"actual_revision"`
				Sync     string  `json:"sync_status"`
				Health   string  `json:"health_status"`
				State    string  `json:"state"`
				Image    string  `json:"image"`
				Reason   string  `json:"reason"`
				Duration *int64  `json:"duration_ms"`
			} `json:"deployment"`
			HealthChecks []struct {
				Attempt   int    `json:"attempt"`
				Status    string `json:"status"`
				HTTPCode  *int   `json:"http_status"`
				LatencyMS *int   `json:"latency_ms"`
				Message   string `json:"message"`
			} `json:"health_checks"`
		}
		if err := c.call("GET", "/api/v1/deployments/"+args[1], nil, &res); err != nil {
			return err
		}
		d := res.Deployment
		w := out()
		fmt.Fprintf(w, "DEPLOYMENT\t#%d\n", d.Number)
		fmt.Fprintf(w, "ENVIRONMENT\t%s\n", d.Env)
		fmt.Fprintf(w, "RELEASE\t%s\n", d.Version)
		fmt.Fprintf(w, "IMAGE\t%s\n", d.Image)
		fmt.Fprintf(w, "DESIRED\t%s\n", d.Desired)
		fmt.Fprintf(w, "ACTUAL\t%s\n", deref(d.Actual))
		fmt.Fprintf(w, "SYNC\t%s\n", d.Sync)
		fmt.Fprintf(w, "HEALTH\t%s\n", d.Health)
		fmt.Fprintf(w, "STATE\t%s\n", d.State)
		fmt.Fprintf(w, "DURATION\t%s\n", ms(d.Duration))
		fmt.Fprintf(w, "REASON\t%s\n", d.Reason)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "PROBE\tRESULT\tSTATUS\tLATENCY\tMESSAGE")
		for _, h := range res.HealthChecks {
			fmt.Fprintf(w, "%02d\t%s\t%s\t%s\t%s\n",
				h.Attempt, h.Status, intOr(h.HTTPCode), latency(h.LatencyMS), h.Message)
		}
		return w.Flush()

	case "rollback":
		if len(args) < 2 {
			return fmt.Errorf("deployment rollback requires an environment id")
		}
		var res struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		if err := c.call("POST", "/api/v1/environments/"+args[1]+"/rollback",
			map[string]string{"reason": "rolled back from the CLI"}, &res); err != nil {
			return err
		}
		fmt.Printf("Rollback %s — deployment %s\n", strings.ToLower(res.State), res.ID)
		return nil
	}
	return errUsage
}

func (c *client) environment(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errUsage
	}
	var res struct {
		Data []struct {
			ID       string  `json:"id"`
			Name     string  `json:"name"`
			Kind     string  `json:"kind"`
			Project  string  `json:"project_name"`
			Revision *string `json:"revision"`
			Actual   *string `json:"actual_revision"`
			Health   *string `json:"health_status"`
			Drift    int     `json:"open_drift"`
		} `json:"data"`
	}
	if err := c.call("GET", "/api/v1/environments", nil, &res); err != nil {
		return err
	}
	w := out()
	fmt.Fprintln(w, "ID\tENVIRONMENT\tPROJECT\tKIND\tDESIRED\tACTUAL\tHEALTH\tDRIFT")
	for _, e := range res.Data {
		drift := ""
		if e.Drift > 0 {
			drift = fmt.Sprintf("%d", e.Drift)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.ID, e.Name, e.Project, strings.ToLower(e.Kind),
			deref(e.Revision), deref(e.Actual), deref(e.Health), drift)
	}
	return w.Flush()
}

// --- formatting -------------------------------------------------------------------

func stateMark(state string) string {
	switch state {
	case "SUCCEEDED":
		return "+"
	case "FAILED", "TIMED_OUT":
		return "x"
	case "RUNNING":
		return ">"
	case "SKIPPED", "CANCELLED":
		return "-"
	default:
		return "."
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func deref(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func intOr(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *n)
}

func latency(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *n)
}

func ms(d *int64) string {
	if d == nil {
		return "-"
	}
	seconds := float64(*d) / 1000
	if seconds < 60 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	return fmt.Sprintf("%dm %02ds", int(seconds)/60, int(seconds)%60)
}

func runRef(number *int64, state *string) string {
	if number == nil {
		return "-"
	}
	if state == nil {
		return fmt.Sprintf("#%d", *number)
	}
	return fmt.Sprintf("#%d %s", *number, strings.ToLower(*state))
}
