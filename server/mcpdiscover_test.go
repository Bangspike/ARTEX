package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

func TestMCPTaskIdentityTemplate(t *testing.T) {
	m := &db.MCPServer{Env: json.RawMessage(`{"X-ARTEX-Task-ID":"{{task_id}}","ARTEX_TASK_ID":"{{task_id}}","Authorization":"Bearer literal-token","unrelated":"{{task_id}}"}`)}
	for _, tc := range []struct {
		name string
		ri   agent.RunInfo
		want string
	}{{"task", agent.RunInfo{TaskID: 42}, "42"}, {"chat", agent.RunInfo{SessionID: "session-1"}, "chat:session-1"}, {"discovery", agent.RunInfo{}, "discovery"}} {
		t.Run(tc.name, func(t *testing.T) {
			env := mcpEnvForRun(agent.WithRunInfo(context.Background(), tc.ri), m)
			if env["X-ARTEX-Task-ID"] != tc.want || env["ARTEX_TASK_ID"] != tc.want {
				t.Fatal(env)
			}
			if env["Authorization"] != "Bearer literal-token" || env["unrelated"] != "{{task_id}}" {
				t.Fatal("unrelated configuration expanded")
			}
			env["Authorization"] = "modified"
			if mcpEnvForRun(context.Background(), m)["Authorization"] != "Bearer literal-token" {
				t.Fatal("configuration mutated")
			}
		})
	}
}
func TestMCPFixedIdentityIsPreserved(t *testing.T) {
	m := &db.MCPServer{Env: json.RawMessage(`{"X-ARTEX-Task-ID":"fixed-task"}`)}
	env := mcpEnvForRun(agent.WithRunInfo(context.Background(), agent.RunInfo{TaskID: 42}), m)
	if env["X-ARTEX-Task-ID"] != "fixed-task" {
		t.Fatal(env)
	}
}
