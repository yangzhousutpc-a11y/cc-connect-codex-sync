package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/yangzhousutpc-a11y/cc-connect-codex-sync/core"
)

func TestAgentImplementsRecoverableAgentSessionStarter(t *testing.T) {
	var agent any = &Agent{}
	if _, ok := agent.(core.RecoverableAgentSessionStarter); !ok {
		t.Fatal("Codex agent does not implement RecoverableAgentSessionStarter")
	}
}

func TestStartRecoverableSession_RejectsExecBackend(t *testing.T) {
	agent := &Agent{backend: "exec"}
	_, err := agent.StartRecoverableSession(context.Background(), "", "cc-connect/new/op-1")
	if err == nil || !core.IsPermanentOperationError(err) || !strings.Contains(err.Error(), "app_server") {
		t.Fatalf("StartRecoverableSession() error = %v, want permanent app_server requirement", err)
	}
}
