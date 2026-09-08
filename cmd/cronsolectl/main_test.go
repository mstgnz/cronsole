package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// marshalable reports whether the declaration survives encoding/json, which is
// what the command does with it.
func marshalable(v any) ([]byte, error) { return json.Marshal(v) }

func TestLiftPositional(t *testing.T) {
	// Go's flag package stops at the first non-flag argument, so without this
	// `run <code> --wait` parses the code and then silently ignores --wait: the
	// command reports the run as queued and exits 0 whatever the run did. That
	// is the worst shape of bug for a pipeline, so it is pinned here.
	cases := []struct {
		name     string
		args     []string
		wantArg  string
		wantRest []string
	}{
		{
			name:     "code before its flags",
			args:     []string{"daily-report", "--wait", "--timeout", "60s"},
			wantArg:  "daily-report",
			wantRest: []string{"--wait", "--timeout", "60s"},
		},
		{
			name:     "code after its flags, which flag parses on its own",
			args:     []string{"--wait", "daily-report"},
			wantArg:  "",
			wantRest: []string{"--wait", "daily-report"},
		},
		{
			name:     "nothing",
			args:     nil,
			wantArg:  "",
			wantRest: nil,
		},
		{
			// A bare word after a flag is not lifted: in `--timeout 60s` the
			// 60s belongs to the flag, and guessing would be worse than not.
			name:     "only a leading word is lifted",
			args:     []string{"--timeout", "60s", "daily-report"},
			wantArg:  "",
			wantRest: []string{"--timeout", "60s", "daily-report"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arg, rest := liftPositional(tc.args)
			if arg != tc.wantArg {
				t.Errorf("positional = %q, want %q", arg, tc.wantArg)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestReadDeclaration(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("yaml becomes a JSON shaped map", func(t *testing.T) {
		// yaml.v2 decodes nested mappings as map[any]any, which encoding/json
		// refuses outright. Without the conversion the command fails at the
		// point of sending, with an error about a map key type that says
		// nothing about the file the person wrote.
		path := write("cron.yaml", `
jobs:
  - code: daily-report
    url: /cron/report
    schedules: ["0 3 * * *"]
    links:
      - target: cleanup
        delay_sec: 30
`)
		doc, err := readDeclaration(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := marshalable(doc); err != nil {
			t.Fatalf("the declaration cannot be sent as JSON: %v", err)
		}

		jobs, ok := doc["jobs"].([]any)
		if !ok || len(jobs) != 1 {
			t.Fatalf("jobs = %#v", doc["jobs"])
		}
		job, ok := jobs[0].(map[string]any)
		if !ok {
			t.Fatalf("a job decoded as %T, want map[string]any", jobs[0])
		}
		links, ok := job["links"].([]any)
		if !ok || len(links) != 1 {
			t.Fatalf("links = %#v", job["links"])
		}
		if _, ok := links[0].(map[string]any); !ok {
			t.Errorf("a nested link decoded as %T, want map[string]any", links[0])
		}
	})

	t.Run("json is accepted too", func(t *testing.T) {
		path := write("cron.json", `{"jobs":[{"code":"a","url":"/a"}]}`)
		if _, err := readDeclaration(path); err != nil {
			t.Fatalf("a JSON declaration was refused: %v", err)
		}
	})

	t.Run("a file with no jobs is refused", func(t *testing.T) {
		// Rather than posting an empty payload, which with --prune would
		// deactivate the project's entire schedule.
		path := write("empty.yaml", "prune: true\n")
		if _, err := readDeclaration(path); err == nil {
			t.Error("a declaration with no jobs was accepted")
		}
	})

	t.Run("a missing file names itself", func(t *testing.T) {
		_, err := readDeclaration(filepath.Join(dir, "absent.yaml"))
		if err == nil {
			t.Fatal("a missing file was accepted")
		}
	})
}
