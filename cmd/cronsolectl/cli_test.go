package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The client is driven the way a deploy pipeline drives it: through run(), with
// a real HTTP server on the other end. What a pipeline actually reads is the
// EXIT CODE, so that is what most of these assert. "Eleven of twelve
// registered" answering zero is the failure mode the codes exist to prevent.

// server is a stand-in Cronsole deployment.
type server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []recorded
	// handler answers each request. Set per test.
	handler func(w http.ResponseWriter, r *http.Request)
}

type recorded struct {
	method string
	path   string
	key    string
	body   string
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *server {
	t.Helper()

	s := &server{handler: handler}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)

		s.mu.Lock()
		s.requests = append(s.requests, recorded{
			method: r.Method, path: r.URL.Path + "?" + r.URL.RawQuery,
			key: r.Header.Get("X-API-Key"), body: body.String(),
		})
		s.mu.Unlock()

		s.handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) seen() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

// answer writes an envelope in the shape the API uses.
func answer(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": code < 400, "data": data})
}

// jobList and runList are the shapes the list endpoints answer with: rows under
// a named key, beside a total. A bare array here would let a test pass against
// a response the deployment never sends.
func jobList(rows ...map[string]any) map[string]any {
	if rows == nil {
		rows = []map[string]any{}
	}
	return map[string]any{"total": len(rows), "jobs": rows}
}

func runList(rows ...map[string]any) map[string]any {
	if rows == nil {
		rows = []map[string]any{}
	}
	return map[string]any{"total": len(rows), "runs": rows}
}

// invoke runs the client and returns its exit code and output.
func invoke(t *testing.T, s *server, key string, args ...string) (int, string, string) {
	t.Helper()

	if s != nil {
		t.Setenv("CRONSOLE_URL", s.URL)
	} else {
		t.Setenv("CRONSOLE_URL", "")
	}
	t.Setenv("CRONSOLE_API_KEY", key)

	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// writeFile puts a declaration in a temporary directory.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// --- the command line itself ------------------------------------------------

func TestNoArgumentsIsAUsageError(t *testing.T) {
	code, _, stderr := invoke(t, nil, "")
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "cronsolectl") {
		t.Errorf("stderr = %q, want the usage text", stderr)
	}
}

func TestHelpAndVersionSucceed(t *testing.T) {
	// A pipeline that runs `cronsolectl help` to check the binary is there must
	// not read a non-zero code as a broken install.
	for _, arg := range []string{"help", "-h", "--help"} {
		code, stdout, _ := invoke(t, nil, "", arg)
		if code != exitOK {
			t.Errorf("%s exited %d", arg, code)
		}
		if !strings.Contains(stdout, "Exit codes") {
			t.Errorf("%s did not print the exit codes", arg)
		}
	}
	for _, arg := range []string{"version", "-v", "--version"} {
		code, stdout, _ := invoke(t, nil, "", arg)
		if code != exitOK {
			t.Errorf("%s exited %d", arg, code)
		}
		if !strings.Contains(stdout, "cronsolectl") {
			t.Errorf("%s printed %q", arg, stdout)
		}
	}
}

func TestAnUnknownCommandIsAUsageError(t *testing.T) {
	code, _, stderr := invoke(t, nil, "", "deploy")
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "deploy") {
		t.Errorf("stderr = %q, want it to name the command", stderr)
	}
}

func TestMissingCredentialsAreAUsageErrorRatherThanARequest(t *testing.T) {
	// Failing before the request is what tells somebody their environment is
	// wrong, instead of a connection error that reads like an outage.
	t.Setenv("CRONSOLE_URL", "")
	t.Setenv("CRONSOLE_API_KEY", "")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"jobs"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "CRONSOLE_URL") && !strings.Contains(stderr.String(), "url") {
		t.Errorf("stderr = %q, want it to name what is missing", stderr)
	}
}

func TestTheKeyIsReadFromTheEnvironmentByPreference(t *testing.T) {
	// A key on the command line lands in the shell history and in the process
	// list, where anyone on the machine can read it. The environment wins so a
	// pipeline that sets both does the safer thing.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, jobList())
	})

	code, _, stderr := invoke(t, s, "key-from-env", "jobs", "--key", "key-from-flag")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}

	seen := s.seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	if seen[0].key != "key-from-env" {
		t.Errorf("the key sent was %q, want the one from the environment", seen[0].key)
	}
}

// --- jobs -------------------------------------------------------------------

func TestJobsPrintsATable(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, jobList(
			map[string]any{"code": "daily-report", "name": "Daily report", "active": true,
				"schedules": []string{"0 3 * * *"}, "last_status": "success"},
			map[string]any{"code": "cleanup", "name": "Cleanup", "active": false,
				"schedules": []string{}, "last_status": ""},
		))
	})

	code, stdout, stderr := invoke(t, s, "key", "jobs")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{"daily-report", "cleanup", "0 3 * * *"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the table is missing %q:\n%s", want, stdout)
		}
	}
}

func TestJSONOutputIsTheRawResponse(t *testing.T) {
	// For a pipeline that pipes into jq. It has to be parseable, which the
	// table is not.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, jobList(map[string]any{"code": "daily", "active": true}))
	})

	code, stdout, _ := invoke(t, s, "key", "jobs", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var parsed any
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Errorf("--json did not print JSON: %v\n%s", err, stdout)
	}
}

func TestAnEmptyListSaysSoRatherThanPrintingNothing(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, jobList())
	})

	code, stdout, _ := invoke(t, s, "key", "jobs")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("an empty list printed nothing, which reads as a broken command")
	}
}

// --- how failures are reported ----------------------------------------------

func TestServerErrorsBecomeExitOne(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"an invalid key", http.StatusUnauthorized},
		{"a job that is not there", http.StatusNotFound},
		{"a refusal", http.StatusForbidden},
		{"a server fault", http.StatusInternalServerError},
		{"rate limited", http.StatusTooManyRequests},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"status":false,"message":"refused"}`))
			})

			code, _, stderr := invoke(t, s, "key", "jobs")
			if code != exitError {
				t.Errorf("exit = %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr, "refused") {
				t.Errorf("stderr = %q, want the server's message", stderr)
			}
		})
	}
}

func TestAnUnreachableServerIsExitOne(t *testing.T) {
	// A closed port, so the dial fails at once rather than on a timeout.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	url := s.URL
	s.Close()

	t.Setenv("CRONSOLE_URL", url)
	t.Setenv("CRONSOLE_API_KEY", "key")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"jobs"}, &stdout, &stderr); code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if strings.TrimSpace(stderr.String()) == "" {
		t.Error("nothing was written to stderr")
	}
}

func TestAnAnswerThatIsNotJSONIsReportedReadably(t *testing.T) {
	// A proxy in the way answers HTML. Printing the whole page would fill the
	// pipeline log with markup.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>" + strings.Repeat("x", 5000) + "</body></html>"))
	})

	code, _, stderr := invoke(t, s, "key", "jobs")
	if code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
	if len(stderr) > 2000 {
		t.Errorf("stderr is %d bytes; a whole page reached the log", len(stderr))
	}
}

// --- sync -------------------------------------------------------------------

const declaration = `
jobs:
  - code: daily-report
    name: Daily report
    url: /cron/daily-report
    schedules:
      - "0 3 * * *"
  - code: cleanup
    name: Cleanup
    url: /cron/cleanup
`

func TestSyncSendsTheDeclaration(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{
			"created": 2, "updated": 0, "deactivated": 0, "results": []any{},
		})
	})

	path := writeFile(t, "cron.yaml", declaration)
	code, stdout, stderr := invoke(t, s, "key", "sync", "-f", path)
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "2") {
		t.Errorf("stdout does not report what happened:\n%s", stdout)
	}

	seen := s.seen()
	if len(seen) != 1 {
		t.Fatalf("%d requests", len(seen))
	}
	if !strings.Contains(seen[0].path, "/api/v1/sync") {
		t.Errorf("the request went to %q", seen[0].path)
	}
	// The YAML is converted to JSON before it is sent: the API speaks JSON, and
	// the file is YAML because a schedule is easier to read that way.
	var sent map[string]any
	if err := json.Unmarshal([]byte(seen[0].body), &sent); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, seen[0].body)
	}
	jobs, _ := sent["jobs"].([]any)
	if len(jobs) != 2 {
		t.Errorf("%d jobs were sent, want 2", len(jobs))
	}
}

func TestSyncPassesPruneThrough(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"created": 0, "updated": 0, "deactivated": 3})
	})

	path := writeFile(t, "cron.yaml", declaration)
	code, _, stderr := invoke(t, s, "key", "sync", "-f", path, "--prune")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}

	var sent map[string]any
	mustUnmarshal(t, s.seen()[0].body, &sent)
	if sent["prune"] != true {
		t.Errorf("prune = %v, want true", sent["prune"])
	}
}

func TestADryRunSendsNothing(t *testing.T) {
	// Local on purpose. It shows what WOULD be sent, which is what catches the
	// common mistake: a file that parsed differently from how it reads. Asking
	// the server what it would do would make a dry run a request, and a request
	// against production is not a dry run.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a dry run reached the server")
		answer(w, http.StatusOK, map[string]any{})
	})

	path := writeFile(t, "cron.yaml", declaration)
	code, stdout, stderr := invoke(t, s, "key", "sync", "-f", path, "--prune", "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if len(s.seen()) != 0 {
		t.Errorf("%d requests were made", len(s.seen()))
	}

	// And it prints the payload, which is the whole point.
	var printed map[string]any
	mustUnmarshal(t, stdout, &printed)
	if printed["prune"] != true {
		t.Errorf("the printed payload does not carry prune: %s", stdout)
	}
	jobs, _ := printed["jobs"].([]any)
	if len(jobs) != 2 {
		t.Errorf("the printed payload has %d jobs, want 2", len(jobs))
	}
}

func TestSyncExitsThreeOnAPartialApplication(t *testing.T) {
	// THE reason the client exists. A pipeline reading 0 here would treat
	// "eleven of twelve registered" as a clean deploy, and the twelfth job
	// would simply never run.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusMultiStatus, map[string]any{
			"project": "shop", "created": 1, "updated": 0, "deactivated": 0, "failed": 1,
			"jobs": []map[string]any{
				{"code": "daily-report", "action": "created"},
				{"code": "cleanup", "action": "failed",
					"errors": []string{"url is not on the project's host"}},
			},
		})
	})

	path := writeFile(t, "cron.yaml", declaration)
	code, stdout, stderr := invoke(t, s, "key", "sync", "-f", path)

	if code != exitPartial {
		t.Errorf("exit = %d, want %d", code, exitPartial)
	}
	// And it says which one, or the code is a puzzle rather than a report.
	combined := stdout + stderr
	if !strings.Contains(combined, "cleanup") {
		t.Errorf("the output does not name the job that was refused:\n%s", combined)
	}
}

func TestSyncRefusesAFileThatIsNotThere(t *testing.T) {
	code, _, stderr := invoke(t, nil, "key", "sync", "-f", "/nonexistent/cron.yaml")
	if code != exitUsage && code != exitError {
		t.Errorf("exit = %d, want a failure", code)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing was written to stderr")
	}
}

func TestSyncRefusesADeclarationWithNoJobs(t *testing.T) {
	// An empty file with --prune would deactivate everything. Refusing is the
	// only safe reading of "I meant to declare nothing".
	for _, content := range []string{"jobs: []\n", "{}\n", "\n"} {
		path := writeFile(t, "cron.yaml", content)
		code, _, stderr := invoke(t, nil, "key", "sync", "-f", path, "--prune")
		if code == exitOK {
			t.Errorf("a declaration with no jobs was accepted: %q", content)
		}
		if strings.TrimSpace(stderr) == "" {
			t.Errorf("nothing explained the refusal for %q", content)
		}
	}
}

func TestSyncAcceptsJSONAsWellAsYAML(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, map[string]any{"created": 1})
	})

	path := writeFile(t, "cron.json",
		`{"jobs":[{"code":"daily","name":"Daily","url":"/cron/daily"}]}`)
	code, _, stderr := invoke(t, s, "key", "sync", "-f", path)
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
}

func TestSyncRefusesAMalformedFile(t *testing.T) {
	path := writeFile(t, "cron.yaml", "jobs:\n  - code: daily\n   bad indentation\n")
	code, _, stderr := invoke(t, nil, "key", "sync", "-f", path)
	if code == exitOK {
		t.Error("a malformed file was accepted")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing explained what was wrong with the file")
	}
}

// --- run --------------------------------------------------------------------

func TestRunQueuesAJob(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusAccepted, map[string]any{"run_id": 42, "code": "daily"})
	})

	code, stdout, stderr := invoke(t, s, "key", "run", "daily")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "42") {
		t.Errorf("the run id is not in the output:\n%s", stdout)
	}
	if seen := s.seen(); !strings.Contains(seen[0].path, "/jobs/daily/run") {
		t.Errorf("the request went to %q", seen[0].path)
	}
}

func TestRunNeedsACode(t *testing.T) {
	code, _, stderr := invoke(t, nil, "key", "run")
	if code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing said a code is required")
	}
}

func TestRunWaitReportsTheOutcome(t *testing.T) {
	// The flag that makes this useful in a pipeline: exit non-zero when the run
	// did not succeed, which is the thing a deploy actually wants to know.
	cases := []struct {
		status string
		want   int
	}{
		{"success", exitOK},
		{"failed", exitRunBad},
		{"timeout", exitRunBad},
		{"skipped", exitRunBad},
	}

	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			calls := 0
			s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if strings.Contains(r.URL.Path, "/run") && r.Method == http.MethodPost {
					answer(w, http.StatusAccepted, map[string]any{"run_id": 7})
					return
				}
				// The poll. First still running, then finished, so the wait
				// loop is actually exercised rather than short-circuited.
				status := c.status
				if calls < 3 {
					status = "running"
				}
				answer(w, http.StatusOK, runList(
					map[string]any{"id": 7, "status": status, "job_code": "daily",
						"duration_ms": 120, "http_status": 200},
				))
			})

			code, stdout, stderr := invoke(t, s, "key", "run", "daily", "--wait", "--timeout", "20s")
			if code != c.want {
				t.Errorf("a %s run exited %d, want %d: %s%s", c.status, code, c.want, stdout, stderr)
			}
			if !strings.Contains(stdout+stderr, c.status) {
				t.Errorf("the outcome %q is not in the output:\n%s%s", c.status, stdout, stderr)
			}
		})
	}
}

func TestRunWaitGivesUpOnItsTimeout(t *testing.T) {
	// A run that never finishes must not hold a pipeline open forever.
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			answer(w, http.StatusAccepted, map[string]any{"run_id": 7})
			return
		}
		answer(w, http.StatusOK, runList(map[string]any{"id": 7, "status": "running"}))
	})

	code, _, stderr := invoke(t, s, "key", "run", "daily", "--wait", "--timeout", "2s")
	if code == exitOK {
		t.Error("a run that never finished exited 0")
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("nothing explained that the wait timed out")
	}
}

// --- runs -------------------------------------------------------------------

func TestRunsPrintsTheHistory(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, runList(
			map[string]any{"id": 1, "job_code": "daily", "status": "success",
				"duration_ms": 120, "http_status": 200, "created_at": "2026-09-08T03:00:00Z"},
			map[string]any{"id": 2, "job_code": "cleanup", "status": "failed",
				"duration_ms": 90, "http_status": 500, "created_at": "2026-09-08T04:00:00Z"},
		))
	})

	code, stdout, stderr := invoke(t, s, "key", "runs")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{"daily", "cleanup", "success", "failed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the table is missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunsPassesItsFiltersThrough(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		answer(w, http.StatusOK, runList())
	})

	code, _, stderr := invoke(t, s, "key", "runs", "--status", "failed", "--limit", "5")
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}

	path := s.seen()[0].path
	for _, want := range []string{"status=failed", "limit=5"} {
		if !strings.Contains(path, want) {
			t.Errorf("the request %q is missing %q", path, want)
		}
	}
}

func mustUnmarshal(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, body)
	}
}
