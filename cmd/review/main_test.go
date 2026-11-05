package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPrintsJSONAndExitCodes(t *testing.T) {
	db := filepath.Join(t.TempDir(), "review.sqlite")
	cases := []struct {
		args []string
		code int
		key  string
	}{
		{[]string{"help"}, 0, "usage"},
		{[]string{"run", "--help"}, 0, "usage"},
		{[]string{"run", "--db", db, "--repo", ".", "--patch", "-"}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--base", "HEAD", "--head", "HEAD", "--patch", "x"}, 2, "error"},
		{[]string{"nope"}, 2, "error"},
		{[]string{"coverage", "--db", db, "--repo", "."}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--working-tree", "--with-repo", "bad"}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--working-tree", "--passes", "defect,lint"}, 2, "error"},
		{[]string{"add-finding", "run", "--db", db, "--file", "a.rb"}, 2, "error"},
		{[]string{"add-finding", "run", "--db", db, "--file", "a.rb", "--line", "1", "--title", "t", "--origin", "model"}, 2, "error"},
		{[]string{"add-finding", "missing", "--db", db, "--file", "a.rb", "--line", "1", "--title", "t"}, 1, "error"},
		{[]string{"add-finding", "run", "--db", db, "--file", "a.rb", "--line", "1", "--title", "t", "--verdict", "오탐"}, 2, "error"},
		{[]string{"add-finding", "run", "--db", db, "--file", "a.rb", "--line", "1", "--title", "t", "--origin", "agent", "--verdict", "유용"}, 2, "error"},
		{[]string{"run", "--db", db}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--base", "a"}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--working-tree", "--pr", "1"}, 2, "error"},
		{[]string{"run", "--db", db, "--repo", ".", "--working-tree", "--effort", "max"}, 2, "error"},
		{[]string{"judge", "x", "maybe", "--db", db}, 2, "error"},
		{[]string{"judge", "missing", "결함", "--db", db}, 1, "error"},
		{[]string{"show", "missing", "--db", db}, 1, "error"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		code := run(context.Background(), c.args, strings.NewReader(""), &out, false)
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%v: output is not JSON: %s", c.args, out.String())
		}
		if code != c.code || doc[c.key] == nil {
			t.Errorf("%v: code=%d want %d, output %s", c.args, code, c.code, out.String())
		}
	}
	var text bytes.Buffer
	if code := run(context.Background(), []string{"--help"}, strings.NewReader(""), &text, true); code != 0 || !strings.HasPrefix(text.String(), "review — ") {
		t.Fatalf("terminal help must be plain text: %q", text.String())
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"list", "--db", db}, strings.NewReader(""), &out, false); code != 0 || out.String() != "[]\n" {
		t.Fatalf("empty list: code=%d out=%q", code, out.String())
	}
}
