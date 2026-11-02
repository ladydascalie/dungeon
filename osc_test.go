package main

import (
	"encoding/base64"
	"strings"
	"testing"
	"testing/iotest"
)

func oscSeq(body string) string {
	return oscStatusPrefix + body + "\x1b\\"
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestOSCSinkKeepsTheLastRootRecord(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMUX", "test-server")
	input := oscSeq("state=working:app=pi:msg="+b64("Reading files")) +
		oscSeq("state=done:app=pi:msg="+b64("Finished"))
	if err := runOSCSink(strings.NewReader(input), "test-server", "%1"); err != nil {
		t.Fatalf("runOSCSink: %v", err)
	}
	got, ok := readOSCStatus("%1")
	if !ok {
		t.Fatal("no status written")
	}
	if got.State != "done" || got.App != "pi" || got.Msg != "Finished" {
		t.Fatalf("status %+v", got)
	}
	blocked, ok := parseOSC7501("state=blocked:kind=permission:app=pi:msg="+b64("Approve the command?"), "%9")
	if !ok || blocked.Kind != "permission" || blocked.Msg != "Approve the command?" {
		t.Fatalf("blocked status %+v ok=%v", blocked, ok)
	}
}

func TestOSCSinkHandlesOneByteAtATime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMUX", "test-server")
	input := oscSeq("state=done:app=pi:msg=" + b64("Finished"))
	if err := runOSCSink(iotest.OneByteReader(strings.NewReader(input)), "test-server", "%2"); err != nil {
		t.Fatalf("runOSCSink: %v", err)
	}
	got, ok := readOSCStatus("%2")
	if !ok || got.State != "done" || got.Msg != "Finished" {
		t.Fatalf("status %+v ok=%v", got, ok)
	}
}

func TestOSCSinkClearAndProcessExit(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMUX", "test-server")
	if err := runOSCSink(strings.NewReader(oscSeq("state=done:app=pi")), "test-server", "%3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOSCStatus("%3"); !ok {
		t.Fatal("done status should survive EOF")
	}
	if err := runOSCSink(strings.NewReader(oscSeq("state=clear")), "test-server", "%3"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOSCStatus("%3"); ok {
		t.Fatal("clear did not remove the record")
	}
	if err := runOSCSink(strings.NewReader(oscSeq("state=working:app=pi")), "test-server", "%4"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOSCStatus("%4"); ok {
		t.Fatal("a working record must die with its process")
	}
}

func TestOSCParseRejectsMalformedAndChildRecords(t *testing.T) {
	if _, ok := parseOSC7501("app=pi", "%1"); ok {
		t.Fatal("a report without state must be ignored")
	}
	if _, ok := parseOSC7501("state=working:id=child:app=pi", "%1"); ok {
		t.Fatal("a child record must not overwrite the root")
	}
	if _, ok := parseOSC7501("state=working:msg=not-base64!", "%1"); ok {
		t.Fatal("a bad base64 message must discard the report")
	}
	if st, ok := parseOSC7501("state=working:progress=40:app=pi", "%1"); !ok || st.Progress == nil || *st.Progress != 40 {
		t.Fatalf("progress %+v ok=%v", st, ok)
	}
}
