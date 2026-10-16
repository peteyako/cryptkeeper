package main

import (
	"flag"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	replace := fs.Bool("replace", false, "")
	data := fs.String("data", "data", "")
	if err := parseInterspersed(fs, []string{"file.yaml", "-replace", "-data", "/x"}); err != nil {
		t.Fatal(err)
	}
	if !*replace || *data != "/x" || !slices.Equal(fs.Args(), []string{"file.yaml"}) {
		t.Fatalf("replace=%v data=%q args=%v", *replace, *data, fs.Args())
	}
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	fs2.Bool("replace", false, "")
	if err := parseInterspersed(fs2, []string{"-replace", "a.yaml", "b.yaml"}); err != nil || !slices.Equal(fs2.Args(), []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("%v %v", err, fs2.Args())
	}
}
