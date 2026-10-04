package main

import (
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestParseRejectArgs(t *testing.T) {
	for _, args := range [][]string{
		{"1", "2", "--reason", " incorrect source "},
		{"--reason", "incorrect source", "1", "2"},
		{"1", "--reason=incorrect source", "2"},
	} {
		ids, reason, err := parseRejectArgs(args)
		if err != nil || !reflect.DeepEqual(ids, []int{1, 2}) || reason != "incorrect source" {
			t.Fatalf("parseRejectArgs(%v) = %v, %q, %v", args, ids, reason, err)
		}
	}
	ids, reason, err := parseRejectArgs([]string{"3"})
	if err != nil || !reflect.DeepEqual(ids, []int{3}) || reason != "" {
		t.Fatalf("pending default = %v, %q, %v", ids, reason, err)
	}
	for _, args := range [][]string{nil, {"0"}, {"-1"}, {"bad"}, {"1", "--reason"}, {"1", "--unknown"}, {"--reason", "text"}} {
		if _, _, err := parseRejectArgs(args); err == nil {
			t.Errorf("parseRejectArgs(%v) succeeded", args)
		}
	}
}

func TestRejectMemoriesPropagatesAPIError(t *testing.T) {
	if err := rejectMemories(nil, []string{"1", "--reason", "incorrect"}); err == nil {
		t.Fatal("rejectMemories hid the kernel error")
	}
}

func TestRejectCLIExit(t *testing.T) {
	if os.Getenv("LEMN_REJECT_CLI_HELPER") == "1" {
		os.Args = append([]string{"lemn", "reject"}, os.Args[3:]...)
		main()
		return
	}
	for _, args := range [][]string{nil, {"bad"}, {"1", "--unknown"}, {"1", "--reason"}, {"1", "--reason", "incorrect"}} {
		commandArgs := append([]string{"-test.run=^TestRejectCLIExit$", "--"}, args...)
		command := exec.Command(os.Args[0], commandArgs...)
		command.Env = append(os.Environ(), "LEMN_REJECT_CLI_HELPER=1", "LEMN_POSTGRES_DSN=host=127.0.0.1 port=1 user=test dbname=test sslmode=disable connect_timeout=1")
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("reject %v exited successfully: %s", args, output)
		}
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("reject %v error = %v, output = %s", args, err, output)
		}
	}
}
