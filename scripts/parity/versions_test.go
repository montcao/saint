package main

import (
	"encoding/json"
	"testing"
)

func TestVersionEvidenceKeepsInstalledAndRunningDistinct(t *testing.T) {
	r := runReport{InstalledHostClaudeVersion: "2.1.4", HostAgentVersion: "2.1.3", SaintAgentVersion: "2.1.4"}
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var got runReport
	if e = json.Unmarshal(b, &got); e != nil {
		t.Fatal(e)
	}
	if got.HostAgentVersion == got.InstalledHostClaudeVersion {
		t.Fatal("running agent inferred from installation")
	}
	if e := validateOptions(options{hostAgentVersion: "latest"}); e == nil {
		t.Fatal("unvalidated observed version accepted")
	}
}
