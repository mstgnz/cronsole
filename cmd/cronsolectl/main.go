// Command cronsolectl talks to a Cronsole deployment from a shell or a
// pipeline.
//
// It exists for the deploy step. A project keeps its schedule in its own
// repository and posts it on deploy; doing that with curl means hand written
// JSON, a jq expression to read the result, and a `grep -qx 200` to notice that
// 207 is a success at the HTTP level and a failure at yours. This does those
// three things properly and gets the exit code right.
//
// Deliberately one file and one dependency beyond the standard library. A CLI
// that a pipeline installs should be a single static binary that does not
// surprise anyone.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v2"
)

// version identifies the build, set with
// -ldflags "-X main.version=$(git rev-parse --short HEAD)".
var version = "dev"

// Exit codes. A pipeline switches on these, so they are part of the contract
// and each one means exactly one thing.
const (
	exitOK      = 0
	exitError   = 1 // the request failed, or the server refused it
	exitUsage   = 2 // the command line was wrong
	exitPartial = 3 // sync applied some jobs and refused others
	exitRunBad  = 4 // --wait, and the run did not succeed
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "cronsolectl", version)
		return exitOK
	case "sync":
		return cmdSync(rest, stdout, stderr)
	case "jobs":
		return cmdJobs(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "runs":
		return cmdRuns(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", command)
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `cronsolectl - register and run scheduled jobs on a Cronsole deployment

  cronsolectl sync [-f cron.yaml] [--prune] [--dry-run]
      Register this project's whole schedule. Idempotent: the same file twice
      changes nothing the second time. An omitted field keeps the stored value,
      so a timeout raised on the screen survives the next deploy.

  cronsolectl jobs
      This project's jobs.

  cronsolectl run <code> [--wait] [--timeout 5m]
      Queue a run. With --wait, block until it finishes and exit non-zero if it
      did not succeed, which is what a pipeline actually wants to know.

  cronsolectl runs [--status failed] [--limit 20] [--since 24h]
      Recent runs.

Credentials, in this order:
  --url and --key, then CRONSOLE_URL and CRONSOLE_API_KEY.

The key is a PROJECT key, so everything here is confined to that project.

Exit codes:
  0 fine   1 failed   2 bad usage   3 sync partially applied   4 run did not succeed
`)
}

// --- shared flags and transport -------------------------------------------

type client struct {
	baseURL string
	key     string
	http    *http.Client
	json    bool
}

// bind adds the flags every command shares and returns the client they build.
//
// The key is read from the environment by preference: a key on the command line
// lands in the shell history and in the process list, where anyone on the
// machine can read it.
func bind(fs *flag.FlagSet) func() (*client, error) {
	var (
		url     = fs.String("url", "", "the deployment, e.g. https://cron.example.com (or CRONSOLE_URL)")
		key     = fs.String("key", "", "a project API key (or CRONSOLE_API_KEY, which is preferred)")
		asJSON  = fs.Bool("json", false, "print the raw response rather than a table")
		timeout = fs.Duration("http-timeout", 30*time.Second, "how long one request may take")
	)

	return func() (*client, error) {
		base := firstNonEmpty(*url, os.Getenv("CRONSOLE_URL"))
		token := firstNonEmpty(os.Getenv("CRONSOLE_API_KEY"), *key)

		if base == "" {
			return nil, errors.New("no deployment address: pass --url or set CRONSOLE_URL")
		}
		if token == "" {
			return nil, errors.New("no key: set CRONSOLE_API_KEY, or pass --key")
		}

		return &client{
			baseURL: strings.TrimRight(base, "/"),
			key:     token,
			json:    *asJSON,
			// Never http.DefaultClient: it has no timeout, so a deployment that
			// accepts the connection and then goes quiet hangs the pipeline
			// until CI kills the job.
			http: &http.Client{
				Timeout: *timeout,
				Transport: &http.Transport{
					DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
					TLSHandshakeTimeout:   5 * time.Second,
					ResponseHeaderTimeout: 15 * time.Second,
				},
			},
		}, nil
	}
}

// liftPositional pulls a leading non-flag argument out, so a command can accept
// it before its flags.
//
// Only a LEADING one. Anything later could be a flag's value: in
// `--timeout 60s foo`, `60s` belongs to --timeout, and guessing which bare word
// is which is how a CLI ends up doing the wrong thing quietly.
func liftPositional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// envelope is the shape every response has.
type envelope struct {
	Status  bool            `json:"status"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Errors  json.RawMessage `json:"errors"`
}

// call sends one request and decodes the envelope.
//
// A non-2xx is returned as an error carrying the server's own message, because
// that message names the rule that refused: "this link already exists" is worth
// far more to somebody reading CI output than "HTTP 422".
func (c *client) call(ctx context.Context, method, path string, body any) (*envelope, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("User-Agent", "cronsolectl/"+version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s could not be reached: %w", c.baseURL, err)
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not the envelope. Usually a proxy or a login page in front of the
		// deployment, so say what arrived rather than "invalid character '<'".
		return nil, res.StatusCode, fmt.Errorf("HTTP %d, and the answer was not from Cronsole: %s",
			res.StatusCode, firstLine(raw))
	}
	if res.StatusCode >= 400 {
		return &env, res.StatusCode, fmt.Errorf("HTTP %d: %s", res.StatusCode, env.Message)
	}
	return &env, res.StatusCode, nil
}

func firstLine(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// --- sync -------------------------------------------------------------------

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	build := bind(fs)
	file := fs.String("f", "cron.yaml", "the declaration to send; YAML or JSON, - for stdin")
	prune := fs.Bool("prune", false, "deactivate jobs that exist on the server but are missing from the file")
	dryRun := fs.Bool("dry-run", false, "print the payload that would be sent and stop")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	payload, err := readDeclaration(*file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	payload["prune"] = *prune

	if *dryRun {
		// Local only: it shows what would be sent, and does not ask the server
		// what it would do. Printing the payload is still the thing that catches
		// the common mistake, which is a file that parsed differently from how
		// it reads.
		encoded, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Fprintln(stdout, string(encoded))
		return exitOK
	}

	c, err := build()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	env, status, err := c.call(ctx, http.MethodPost, "/sync", payload)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if c.json {
		fmt.Fprintln(stdout, string(env.Data))
		return codeForSync(status)
	}

	var result struct {
		Project     string `json:"project"`
		Created     int    `json:"created"`
		Updated     int    `json:"updated"`
		Deactivated int    `json:"deactivated"`
		Failed      int    `json:"failed"`
		Jobs        []struct {
			Code   string   `json:"code"`
			Action string   `json:"action"`
			Errors []string `json:"errors"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(env.Data, &result); err != nil {
		fmt.Fprintln(stderr, "the answer could not be read:", err)
		return exitError
	}

	fmt.Fprintf(stdout, "%s: %d created, %d updated, %d deactivated, %d failed\n",
		result.Project, result.Created, result.Updated, result.Deactivated, result.Failed)
	for _, job := range result.Jobs {
		if job.Action != "failed" {
			continue
		}
		fmt.Fprintf(stderr, "  %s refused: %s\n", job.Code, strings.Join(job.Errors, "; "))
	}
	return codeForSync(status)
}

// codeForSync separates "applied" from "partly applied".
//
// 207 is the case worth having a code for: a pipeline that treats it as success
// deploys eleven of twelve jobs and reports green.
func codeForSync(status int) int {
	if status == http.StatusMultiStatus {
		return exitPartial
	}
	return exitOK
}

// readDeclaration reads the file and returns it as a JSON-shaped map.
//
// YAML because a schedule file is read far more often than it is written, and
// JSON has no comments: the reason a job runs at 03:00 belongs next to the
// expression. JSON is accepted too, being a subset.
func readDeclaration(path string) (map[string]any, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Clean(path), err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML or JSON: %w", filepath.Clean(path), err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s is empty", filepath.Clean(path))
	}
	if _, ok := doc["jobs"]; !ok {
		return nil, fmt.Errorf("%s declares no jobs", filepath.Clean(path))
	}

	// yaml.v2 decodes nested mappings as map[any]any, which encoding/json
	// refuses. Converting here rather than at the call site keeps the rest of
	// the command working in ordinary JSON shapes.
	converted, ok := normalise(doc).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not a mapping at the top level", filepath.Clean(path))
	}
	return converted, nil
}

func normalise(v any) any {
	switch value := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[fmt.Sprint(k)] = normalise(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = normalise(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = normalise(item)
		}
		return out
	default:
		return v
	}
}

// --- jobs -------------------------------------------------------------------

func cmdJobs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	build := bind(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	c, err := build()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	env, _, err := c.call(ctx, http.MethodGet, "/jobs", nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if c.json {
		fmt.Fprintln(stdout, string(env.Data))
		return exitOK
	}

	var body struct {
		Jobs []struct {
			Code       string   `json:"code"`
			Name       string   `json:"name"`
			Method     string   `json:"method"`
			URL        string   `json:"url"`
			Active     bool     `json:"active"`
			Schedules  []string `json:"schedules"`
			LastStatus string   `json:"last_status"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		fmt.Fprintln(stderr, "the answer could not be read:", err)
		return exitError
	}
	sort.Slice(body.Jobs, func(i, j int) bool { return body.Jobs[i].Code < body.Jobs[j].Code })

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tSTATE\tSCHEDULE\tLAST\tTARGET")
	for _, job := range body.Jobs {
		state := "inactive"
		if job.Active {
			state = "active"
		}
		schedule := strings.Join(job.Schedules, ", ")
		if schedule == "" {
			schedule = "-"
		}
		last := job.LastStatus
		if last == "" {
			last = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s %s\n", job.Code, state, schedule, last, job.Method, job.URL)
	}
	_ = tw.Flush()
	return exitOK
}

// --- run --------------------------------------------------------------------

func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	build := bind(fs)
	wait := fs.Bool("wait", false, "block until the run finishes, and fail if it did not succeed")
	timeout := fs.Duration("timeout", 5*time.Minute, "with --wait, how long to keep waiting")

	// Go's flag package stops parsing at the first non-flag argument, so
	// `run <code> --wait` would silently ignore --wait. Lifting the code out
	// first makes both orders work, and `run <code> --wait` is the order people
	// actually type.
	code, rest := liftPositional(args)
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if code == "" {
		code = fs.Arg(0)
	}
	if code == "" || fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: cronsolectl run <code> [--wait] [--timeout 5m]")
		return exitUsage
	}

	c, err := build()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	env, _, err := c.call(ctx, http.MethodPost, "/jobs/"+code+"/run", nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}

	var queued struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &queued); err != nil {
		fmt.Fprintln(stderr, "the answer could not be read:", err)
		return exitError
	}
	fmt.Fprintf(stdout, "%s queued as run %d\n", code, queued.RunID)

	if !*wait {
		// Without --wait the run is queued and that is all this command claims.
		// It is not a success report: the job has not executed yet.
		return exitOK
	}
	return waitForRun(c, queued.RunID, *timeout, stdout, stderr)
}

// waitForRun polls until the run reaches a terminal state.
//
// Polling rather than a stream, because a pipeline wants one answer and a
// scheduler run is measured in seconds. The project API has no per-run endpoint,
// so this reads the recent list and picks the id out; the list is small and the
// interval is generous.
func waitForRun(c *client, runID int64, timeout time.Duration, stdout, stderr io.Writer) int {
	deadline := time.Now().Add(timeout)
	// Terminal states. `pending` and `running` are the two that are not.
	terminal := map[string]bool{"success": true, "failed": true, "timeout": true, "skipped": true}

	for {
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "run %d had not finished after %s; it may still be going\n", runID, timeout)
			return exitRunBad
		}
		time.Sleep(2 * time.Second)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		env, _, err := c.call(ctx, http.MethodGet, "/runs?limit=50", nil)
		cancel()
		if err != nil {
			// A blip while waiting is not a verdict on the run. Keep waiting
			// until the deadline rather than reporting a failure the job did
			// not have.
			fmt.Fprintln(stderr, "still waiting:", err)
			continue
		}

		var body struct {
			Runs []struct {
				ID         int64  `json:"id"`
				Status     string `json:"status"`
				DurationMs *int   `json:"duration_ms"`
				HTTPStatus *int   `json:"http_status"`
				Error      string `json:"error"`
			} `json:"runs"`
		}
		if err := json.Unmarshal(env.Data, &body); err != nil {
			continue
		}

		for _, r := range body.Runs {
			if r.ID != runID || !terminal[r.Status] {
				continue
			}
			detail := r.Status
			if r.HTTPStatus != nil {
				detail += fmt.Sprintf(", HTTP %d", *r.HTTPStatus)
			}
			if r.DurationMs != nil {
				detail += fmt.Sprintf(", %dms", *r.DurationMs)
			}
			if r.Status == "success" {
				fmt.Fprintf(stdout, "run %d %s\n", runID, detail)
				return exitOK
			}
			fmt.Fprintf(stderr, "run %d %s\n", runID, detail)
			if r.Error != "" {
				fmt.Fprintln(stderr, " ", r.Error)
			}
			return exitRunBad
		}
	}
}

// --- runs -------------------------------------------------------------------

func cmdRuns(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	build := bind(fs)
	status := fs.String("status", "", "only this outcome: success, failed, timeout or skipped")
	limit := fs.Int("limit", 20, "how many to show")
	since := fs.Duration("since", 24*time.Hour, "how far back to look")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	c, err := build()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}

	query := fmt.Sprintf("/runs?limit=%d&from=%s", *limit,
		time.Now().Add(-*since).Format("2006-01-02"))
	if *status != "" {
		query += "&status=" + *status
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	env, _, err := c.call(ctx, http.MethodGet, query, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitError
	}
	if c.json {
		fmt.Fprintln(stdout, string(env.Data))
		return exitOK
	}

	var body struct {
		Runs []struct {
			ID         int64     `json:"id"`
			JobCode    string    `json:"job_code"`
			Status     string    `json:"status"`
			Trigger    string    `json:"trigger"`
			DurationMs *int      `json:"duration_ms"`
			HTTPStatus *int      `json:"http_status"`
			CreatedAt  time.Time `json:"created_at"`
			Error      string    `json:"error"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(env.Data, &body); err != nil {
		fmt.Fprintln(stderr, "the answer could not be read:", err)
		return exitError
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tJOB\tSTATUS\tHTTP\tTOOK\tTRIGGER\tERROR")
	for _, r := range body.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.CreatedAt.Local().Format("02 Jan 15:04"), r.JobCode, r.Status,
			optionalInt(r.HTTPStatus), optionalMillis(r.DurationMs), r.Trigger,
			firstLine([]byte(r.Error)))
	}
	_ = tw.Flush()
	return exitOK
}

func optionalInt(v *int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

func optionalMillis(v *int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *v)
}
